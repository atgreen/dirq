# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Anthony Green <green@moxielogic.com>

"""
DirQ fact cache plugin for Ansible.

When used with the DirQ inventory plugin, this provides instant fact
gathering — the agent's cached facts are served from the DirQ server
instead of running the setup module over the connection. This makes
gather_facts: true near-instant.

Configure in ansible.cfg:
    [defaults]
    fact_caching = atgreen.dirq.dirq
    fact_caching_connection = http://dirq-server:8080
"""

from __future__ import annotations

import json
import os

from ansible.plugins.cache import BaseCacheModule

DOCUMENTATION = """
    name: atgreen.dirq.dirq
    short_description: DirQ fact cache
    description:
        - Uses DirQ server as a fact cache backend.
        - Facts are served from agent-side collection, not from running
          the setup module. Makes gather_facts near-instant.
    options:
        _uri:
            description: DirQ server URL
            required: true
            env:
                - name: DIRQ_SERVER_URL
"""


class CacheModule(BaseCacheModule):
    """Fact cache backed by the DirQ server API."""

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._server_url = os.environ.get("DIRQ_SERVER_URL", "http://localhost:8080")
        self._token = os.environ.get("DIRQ_TOKEN", "")
        self._cache = {}
        self._loaded = False

    def _load(self):
        if self._loaded:
            return

        from ansible_collections.atgreen.dirq.plugins.module_utils.api import DirQClient
        client = DirQClient(self._server_url, self._token)

        try:
            inv = client.get("/api/v1/inventory")
        except Exception:
            self._loaded = True
            return

        hostvars = inv.get("_meta", {}).get("hostvars", {})

        for hostname, dv in hostvars.items():
            facts = self._map_facts(hostname, dv)
            self._cache[hostname] = facts

        self._loaded = True

    def _map_facts(self, hostname, dv):
        """Map DirQ data to standard Ansible facts."""
        os_type = dv.get("dirq_os", "linux")
        arch = dv.get("dirq_arch", "amd64")

        arch_map = {"amd64": "x86_64", "arm64": "aarch64", "386": "i386"}

        facts = {
            "ansible_hostname": hostname,
            "ansible_fqdn": hostname,
            "ansible_system": "Linux" if os_type == "linux" else "Win32NT",
            "ansible_os_family": "Windows" if os_type == "windows" else "Linux",
            "ansible_architecture": arch_map.get(arch, arch),
            "ansible_machine": arch_map.get(arch, arch),
        }

        # CPU
        cpu = dv.get("dirq_cpu", {})
        if cpu:
            facts["ansible_processor_vcpus"] = cpu.get("logical_cores", 0)
            facts["ansible_processor_cores"] = cpu.get("physical_cores", 0)
            facts["ansible_processor"] = [cpu.get("vendor", ""), cpu.get("model_name", "")]

        # Memory
        mem = dv.get("dirq_memory", {})
        if mem:
            facts["ansible_memtotal_mb"] = int(mem.get("total_bytes", 0) / 1048576)
            facts["ansible_memfree_mb"] = int(mem.get("available_bytes", 0) / 1048576)
            facts["ansible_swaptotal_mb"] = int(mem.get("swap_total_bytes", 0) / 1048576)

        # OS info
        os_info = dv.get("dirq_os_info") or {}
        if isinstance(os_info, dict):
            facts["ansible_kernel"] = os_info.get("kernel_version", "")
            facts["ansible_uptime_seconds"] = os_info.get("uptime_seconds", 0)
            facts["ansible_distribution_version"] = os_info.get("os_version", "")

        # Network
        net = dv.get("dirq_network", {})
        if isinstance(net, dict):
            interfaces = net.get("interfaces", [])
            iface_dict = {}
            all_addrs = []
            for iface in interfaces:
                if isinstance(iface, dict):
                    name = iface.get("name", "")
                    iface_dict[name] = {
                        "macaddress": iface.get("mac", ""),
                        "mtu": iface.get("mtu", 0),
                    }
                    for addr in iface.get("addresses", []):
                        if isinstance(addr, dict) and addr.get("family") == "IPv4":
                            ip = addr.get("addr", "").split("/")[0]
                            if ip:
                                all_addrs.append(ip)
                                iface_dict[name]["ipv4"] = {"address": ip}
            facts["ansible_interfaces"] = list(iface_dict.keys())
            if all_addrs:
                facts["ansible_default_ipv4"] = {"address": all_addrs[0]}
                facts["ansible_all_ipv4_addresses"] = all_addrs

        # Packages as ansible_facts.packages (Ansible package_facts format)
        pkgs = dv.get("dirq_packages", {})
        if isinstance(pkgs, dict):
            pkg_list = pkgs.get("packages", [])
            pkg_facts = {}
            for p in pkg_list:
                if isinstance(p, dict):
                    name = p.get("name", "")
                    if name:
                        pkg_facts.setdefault(name, []).append({
                            "name": name,
                            "version": p.get("version", ""),
                            "arch": p.get("arch", ""),
                            "source": p.get("source", ""),
                        })
            facts["ansible_facts"] = facts.get("ansible_facts", {})
            facts["ansible_facts"]["packages"] = pkg_facts

        # Services as ansible_facts.services (Ansible service_facts format)
        svcs = dv.get("dirq_services", {})
        if isinstance(svcs, dict):
            svc_list = svcs.get("services", [])
            svc_facts = {}
            for s in svc_list:
                if isinstance(s, dict):
                    name = s.get("name", "")
                    if name:
                        svc_facts[name] = {
                            "name": name,
                            "state": s.get("state", "unknown"),
                            "status": s.get("start_type", "unknown"),
                        }
            if "ansible_facts" not in facts:
                facts["ansible_facts"] = {}
            facts["ansible_facts"]["services"] = svc_facts

        return facts

    def get(self, key):
        self._load()
        return self._cache.get(key, {})

    def set(self, key, value):
        self._cache[key] = value

    def keys(self):
        self._load()
        return list(self._cache.keys())

    def contains(self, key):
        self._load()
        return key in self._cache

    def delete(self, key):
        self._cache.pop(key, None)

    def flush(self):
        self._cache.clear()
        self._loaded = False

    def copy(self):
        self._load()
        return dict(self._cache)
