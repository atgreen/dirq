# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Anthony Green <green@moxielogic.com>

"""
DirQ dynamic inventory plugin for Ansible / AAP.

Sets ansible_connection automatically for exec-enabled hosts, maps DirQ
facts to standard Ansible variables, and routes by stable agent ID.
Existing playbooks work without modification.
"""

from __future__ import annotations

import os

from ansible.errors import AnsibleParserError
from ansible.plugins.inventory import BaseInventoryPlugin

DOCUMENTATION = """
    name: atgreen.dirq.dirq
    plugin_type: inventory
    short_description: DirQ dynamic inventory
    description:
        - Fetches hosts and facts from a DirQ server.
        - Creates groups by OS, architecture, tags, and exec capability.
        - Sets ansible_connection automatically for exec-enabled hosts.
        - Maps DirQ facts to standard Ansible variables (ansible_os_family, etc.).
        - Routes by stable dirq_agent_id, not hostname string matching.
    extends_documentation_fragment:
        - inventory_cache
    options:
        plugin:
            description: Must be atgreen.dirq.dirq
            required: true
            choices: ['atgreen.dirq.dirq']
        server_url:
            description: DirQ server REST API URL.
            required: true
            type: str
            env:
                - name: DIRQ_SERVER_URL
        token:
            description: DirQ API token.
            required: false
            type: str
            env:
                - name: DIRQ_TOKEN
        query:
            description: >
                Optional DirQ query to filter which hosts appear in the inventory.
                Only hosts that match the query will be included.
            required: false
            type: str
        auto_connection:
            description: >
                Automatically set ansible_connection for exec-enabled hosts.
                When true (default), hosts with exec_enabled=true get
                ansible_connection=atgreen.dirq.dirq so playbooks work
                without specifying connection: dirq.
            required: false
            type: bool
            default: true
"""

EXAMPLES = """
# Minimal — existing playbooks just work:
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080

# Query-filtered:
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080
query: "SELECT os_info.hostname FROM * WHERE packages.name = 'openssl' AND packages.version LIKE '1.%'"

# Disable auto-connection (use SSH/WinRM instead of DirQ for execution):
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080
auto_connection: false
"""

# ─────────────────────────────────────────────────────────
# OS family / distribution mapping
# ─────────────────────────────────────────────────────────

# Maps os_version strings and os to Ansible-standard values.
_OS_FAMILY_MAP = {
    "linux": "Linux",
    "windows": "Windows",
}

# Best-effort distro detection from os_version strings.
# DirQ os_info.os_version comes from gopsutil which returns different things
# per platform. On Fedora it's "44", on Ubuntu "22.04", on RHEL "9.2", etc.
# We map what we can and leave the rest as-is.
_DISTRO_PATTERNS = {
    "fedora": "Fedora",
    "centos": "CentOS",
    "rhel": "RedHat",
    "red hat": "RedHat",
    "ubuntu": "Ubuntu",
    "debian": "Debian",
    "suse": "Suse",
    "sles": "Suse",
    "arch": "Archlinux",
    "amazon": "Amazon",
    "rocky": "Rocky",
    "alma": "AlmaLinux",
    "oracle": "OracleLinux",
}

_ARCH_MAP = {
    "amd64": "x86_64",
    "arm64": "aarch64",
    "386": "i386",
}


def _detect_distro(os_type, os_version, hostname):
    """Best-effort detection of ansible_distribution from DirQ data."""
    if os_type == "windows":
        return "Windows", os_version, "Microsoft"

    # Check os_version and hostname for distro hints.
    search = (os_version + " " + hostname).lower()
    for pattern, distro in _DISTRO_PATTERNS.items():
        if pattern in search:
            return distro, os_version, distro

    return "Linux", os_version, "NA"


class InventoryModule(BaseInventoryPlugin):
    NAME = "atgreen.dirq.dirq"

    def verify_file(self, path):
        if super().verify_file(path):
            return path.endswith((".yml", ".yaml"))
        return False

    def parse(self, inventory, loader, path, cache=True):
        super().parse(inventory, loader, path, cache)
        self._read_config_data(path)

        server_url = self.get_option("server_url") or os.environ.get("DIRQ_SERVER_URL")
        token = self.get_option("token") or os.environ.get("DIRQ_TOKEN", "")
        auto_connection = self.get_option("auto_connection")
        if auto_connection is None:
            auto_connection = True

        if not server_url:
            raise AnsibleParserError("server_url is required")

        from ansible_collections.atgreen.dirq.plugins.module_utils.api import DirQClient
        client = DirQClient(server_url, token)

        try:
            inv = client.get("/api/v1/inventory")
        except Exception as e:
            raise AnsibleParserError(f"Failed to fetch DirQ inventory: {e}")

        hostvars = inv.get("_meta", {}).get("hostvars", {})

        # Query filter.
        query_filter = self.get_option("query")
        if query_filter:
            try:
                result = client.post("/api/v1/query", {"query": query_filter, "timeout": 60})
                matched = {r["hostname"] for r in result.get("results", []) if r.get("success")}
                hostvars = {h: v for h, v in hostvars.items() if h in matched}
            except Exception as e:
                raise AnsibleParserError(f"DirQ query filter failed: {e}")

        # Add hosts.
        for hostname, dv in hostvars.items():
            self.inventory.add_host(hostname)

            # ── DirQ-specific vars (keep as extra metadata) ──
            for k, v in dv.items():
                self.inventory.set_variable(hostname, k, v)

            # ── Stable routing identity ──
            # The connection plugin uses dirq_agent_id to route, not hostname.
            # ansible_host is set to the hostname for display purposes.
            self.inventory.set_variable(hostname, "ansible_host", hostname)

            # Server URL per host (multi-DC routing). Token stays in env/credential.
            self.inventory.set_variable(hostname, "dirq_server_url", server_url)

            # ── Auto-set connection for exec-enabled hosts ──
            if auto_connection and dv.get("dirq_exec_enabled"):
                self.inventory.set_variable(hostname, "ansible_connection", "atgreen.dirq.dirq")

            # ── Map to standard Ansible facts ──
            os_type = dv.get("dirq_os", "linux")
            os_version = dv.get("dirq_os_version", "")
            arch = dv.get("dirq_arch", "amd64")

            # OS family
            self.inventory.set_variable(hostname, "ansible_os_family",
                                        _OS_FAMILY_MAP.get(os_type, os_type.capitalize()))
            self.inventory.set_variable(hostname, "ansible_system",
                                        "Linux" if os_type == "linux" else "Win32NT")

            # Distribution
            distro, distro_version, distro_release = _detect_distro(os_type, os_version, hostname)
            self.inventory.set_variable(hostname, "ansible_distribution", distro)
            self.inventory.set_variable(hostname, "ansible_distribution_version", distro_version)
            self.inventory.set_variable(hostname, "ansible_distribution_release", distro_release)

            # Architecture
            self.inventory.set_variable(hostname, "ansible_architecture",
                                        _ARCH_MAP.get(arch, arch))
            self.inventory.set_variable(hostname, "ansible_machine",
                                        _ARCH_MAP.get(arch, arch))

            # Hostname
            self.inventory.set_variable(hostname, "ansible_hostname", hostname)
            self.inventory.set_variable(hostname, "ansible_fqdn", hostname)

            # ── OS-specific shell and interpreter settings ──
            if os_type == "windows":
                self.inventory.set_variable(hostname, "ansible_shell_type", "powershell")
                self.inventory.set_variable(hostname, "ansible_become_method", "runas")
            else:
                self.inventory.set_variable(hostname, "ansible_shell_type", "sh")
                self.inventory.set_variable(hostname, "ansible_python_interpreter", "/usr/bin/python3")
                self.inventory.set_variable(hostname, "ansible_become_method", "sudo")

            # ── CPU / memory as Ansible facts ──
            cpu = dv.get("dirq_cpu", {})
            if cpu:
                self.inventory.set_variable(hostname, "ansible_processor_vcpus",
                                            cpu.get("logical_cores", 0))
                self.inventory.set_variable(hostname, "ansible_processor_cores",
                                            cpu.get("physical_cores", 0))

            mem = dv.get("dirq_memory", {})
            if mem:
                total_mb = int(mem.get("total_bytes", 0) / 1048576)
                self.inventory.set_variable(hostname, "ansible_memtotal_mb", total_mb)

        # Add groups.
        for group_name, group_data in inv.items():
            if group_name in ("_meta", "all"):
                continue
            if not isinstance(group_data, dict):
                continue

            self.inventory.add_group(group_name)

            for host in group_data.get("hosts", []):
                if host in hostvars:
                    self.inventory.add_host(host, group=group_name)

            for child in group_data.get("children", []):
                self.inventory.add_group(child)
                self.inventory.add_child(group_name, child)
