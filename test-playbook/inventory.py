#!/usr/bin/env python3
# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Anthony Green <green@moxielogic.com>

"""Simple DirQ inventory for local testing. Supports --query filtering."""
import json
import os
import sys
from urllib.request import Request, urlopen

server = os.environ.get("DIRQ_SERVER_URL", "http://localhost:8090")
token = os.environ.get("DIRQ_TOKEN", "")
query_filter = os.environ.get("DIRQ_QUERY", "")

req = Request(server + "/api/v1/inventory")
req.add_header("Content-Type", "application/json")
if token:
    req.add_header("Authorization", "Bearer " + token)
data = json.loads(urlopen(req, timeout=10).read())

# Apply query filter if set.
if query_filter:
    payload = json.dumps({"query": query_filter, "timeout": 60}).encode("utf-8")
    qreq = Request(server + "/api/v1/query", data=payload, method="POST")
    qreq.add_header("Content-Type", "application/json")
    if token:
        qreq.add_header("Authorization", "Bearer " + token)
    result = json.loads(urlopen(qreq, timeout=120).read())
    matched = {r["hostname"] for r in result.get("results", []) if r.get("success")}

    data["_meta"]["hostvars"] = {h: v for h, v in data["_meta"]["hostvars"].items() if h in matched}
    for group_name, group_data in data.items():
        if group_name == "_meta" or not isinstance(group_data, dict):
            continue
        if "hosts" in group_data:
            group_data["hosts"] = [h for h in group_data["hosts"] if h in matched]

if "--list" in sys.argv:
    print(json.dumps(data, indent=2))
elif "--host" in sys.argv:
    host = sys.argv[sys.argv.index("--host") + 1]
    hostvars = data.get("_meta", {}).get("hostvars", {})
    print(json.dumps(hostvars.get(host, {}), indent=2))
