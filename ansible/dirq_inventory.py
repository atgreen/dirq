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


def run_query(server_url, token, query_str):
    """Run a DirQ query and return the set of matching hostnames."""
    url = server_url.rstrip("/") + "/api/v1/query"
    payload = json.dumps({"query": query_str, "timeout": 60}).encode("utf-8")
    req = Request(url, data=payload, method="POST")
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)

    try:
        resp = urlopen(req, timeout=120)
        result = json.loads(resp.read().decode("utf-8"))
        return {r["hostname"] for r in result.get("results", []) if r.get("success")}
    except URLError as e:
        print(f"ERROR: DirQ query failed: {e}", file=sys.stderr)
        sys.exit(1)


def filter_inventory(inventory, matched_hosts):
    """Filter an inventory to only include hosts in matched_hosts."""
    hostvars = inventory.get("_meta", {}).get("hostvars", {})
    filtered_hostvars = {h: v for h, v in hostvars.items() if h in matched_hosts}
    inventory["_meta"]["hostvars"] = filtered_hostvars

    for group_name, group_data in list(inventory.items()):
        if group_name == "_meta" or not isinstance(group_data, dict):
            continue
        if "hosts" in group_data:
            group_data["hosts"] = [h for h in group_data["hosts"] if h in matched_hosts]

    return inventory


def main():
    """Entry point for standalone dynamic inventory script."""
    import argparse

    parser = argparse.ArgumentParser(description="DirQ Ansible dynamic inventory")
    parser.add_argument("--list", action="store_true", help="List all hosts")
    parser.add_argument("--host", type=str, help="Get vars for a specific host")
    parser.add_argument("--query", type=str, help="DirQ query to filter hosts",
                        default=os.environ.get("DIRQ_QUERY", ""))
    args = parser.parse_args()

    server_url = os.environ.get("DIRQ_SERVER_URL", "http://localhost:8080")
    token = os.environ.get("DIRQ_TOKEN", "")

    if args.list:
        inventory = fetch_inventory(server_url, token)
        if args.query:
            matched = run_query(server_url, token, args.query)
            inventory = filter_inventory(inventory, matched)
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
