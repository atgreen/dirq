#!/usr/bin/env python3
# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Anthony Green <green@moxielogic.com>

"""
DirQ dynamic inventory plugin for Ansible / AAP.

Configure in your inventory source YAML:

    # dirq.yml
    plugin: dirq_inventory
    server_url: http://localhost:8080
    token: your-api-token
    # exclude_stale: true   # optional: exclude hosts past TTL

Usage:
    ansible-inventory -i dirq.yml --list
    ansible-playbook -i dirq.yml site.yml
"""

import json
import os
import sys

try:
    from urllib.request import Request, urlopen
    from urllib.error import URLError
except ImportError:
    from urllib2 import Request, urlopen, URLError

DOCUMENTATION = """
    name: dirq_inventory
    plugin_type: inventory
    short_description: DirQ real-time endpoint inventory
    description:
        - Fetches hosts and facts from a DirQ server.
        - All collected data is exposed as Ansible facts under the dirq_* namespace.
    options:
        server_url:
            description: DirQ server HTTP URL.
            required: true
            env:
                - name: DIRQ_SERVER_URL
        token:
            description: DirQ API token.
            required: false
            env:
                - name: DIRQ_TOKEN
"""


def fetch_inventory(server_url, token=None):
    """Fetch the Ansible inventory JSON from the DirQ server."""
    url = server_url.rstrip("/") + "/api/v1/inventory"
    req = Request(url)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)

    try:
        resp = urlopen(req, timeout=30)
        return json.loads(resp.read().decode("utf-8"))
    except URLError as e:
        print(f"ERROR: Failed to fetch inventory from {url}: {e}", file=sys.stderr)
        sys.exit(1)


def main():
    """Entry point for standalone dynamic inventory script."""
    import argparse

    parser = argparse.ArgumentParser(description="DirQ Ansible dynamic inventory")
    parser.add_argument("--list", action="store_true", help="List all hosts")
    parser.add_argument("--host", type=str, help="Get vars for a specific host")
    args = parser.parse_args()

    server_url = os.environ.get("DIRQ_SERVER_URL", "http://localhost:8080")
    token = os.environ.get("DIRQ_TOKEN", "")

    if args.list:
        inventory = fetch_inventory(server_url, token)
        print(json.dumps(inventory, indent=2))
    elif args.host:
        inventory = fetch_inventory(server_url, token)
        hostvars = inventory.get("_meta", {}).get("hostvars", {})
        host_data = hostvars.get(args.host, {})
        print(json.dumps(host_data, indent=2))
    else:
        parser.print_help()
        sys.exit(1)


if __name__ == "__main__":
    main()
