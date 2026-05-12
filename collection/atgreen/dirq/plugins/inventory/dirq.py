# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Anthony Green <green@moxielogic.com>

"""
DirQ dynamic inventory plugin for Ansible / AAP.

To use as an AAP inventory source, create a YAML inventory file:

    # dirq.yml
    plugin: atgreen.dirq.dirq
    server_url: http://dirq-server:8080
    token: "{{ lookup('env', 'DIRQ_TOKEN') }}"

Then add it as an Inventory Source in AAP pointing at this file.
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
        - All collected data exposed as host vars under the dirq_* namespace.
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
"""

EXAMPLES = """
# Minimal inventory file (dirq.yml):
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080

# With token:
plugin: atgreen.dirq.dirq
server_url: http://dirq-server:8080
token: my-secret-token
"""


class InventoryModule(BaseInventoryPlugin):
    NAME = "atgreen.dirq.dirq"

    def verify_file(self, path):
        """Accept .yml/.yaml files that declare our plugin."""
        if super().verify_file(path):
            return path.endswith((".yml", ".yaml"))
        return False

    def parse(self, inventory, loader, path, cache=True):
        super().parse(inventory, loader, path, cache)
        self._read_config_data(path)

        server_url = self.get_option("server_url") or os.environ.get("DIRQ_SERVER_URL")
        token = self.get_option("token") or os.environ.get("DIRQ_TOKEN", "")

        if not server_url:
            raise AnsibleParserError("server_url is required")

        # Import here to avoid issues when module_utils isn't on the path yet.
        from ansible_collections.atgreen.dirq.plugins.module_utils.api import DirQClient

        client = DirQClient(server_url, token)

        try:
            inv = client.get("/api/v1/inventory")
        except Exception as e:
            raise AnsibleParserError(f"Failed to fetch DirQ inventory: {e}")

        hostvars = inv.get("_meta", {}).get("hostvars", {})

        # Add hosts and their vars.
        for hostname, vars_dict in hostvars.items():
            self.inventory.add_host(hostname)
            for k, v in vars_dict.items():
                self.inventory.set_variable(hostname, k, v)
            # Set dirq_server_url per host so the connection plugin knows
            # which DirQ server to route through (critical for multi-DC).
            self.inventory.set_variable(hostname, "dirq_server_url", server_url)
            if token:
                self.inventory.set_variable(hostname, "dirq_token", token)

        # Add groups from the inventory response.
        for group_name, group_data in inv.items():
            if group_name in ("_meta", "all"):
                continue

            if not isinstance(group_data, dict):
                continue

            self.inventory.add_group(group_name)

            # Add hosts to this group.
            for host in group_data.get("hosts", []):
                self.inventory.add_host(host, group=group_name)

            # Add child groups.
            for child in group_data.get("children", []):
                self.inventory.add_group(child)
                self.inventory.add_child(group_name, child)
