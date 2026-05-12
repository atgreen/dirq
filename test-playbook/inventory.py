#!/usr/bin/env python3
"""Simple DirQ inventory for local testing."""
import json
import os
import sys
from urllib.request import Request, urlopen

server = os.environ.get("DIRQ_SERVER_URL", "http://localhost:8090")
req = Request(server + "/api/v1/inventory")
req.add_header("Content-Type", "application/json")
data = json.loads(urlopen(req, timeout=10).read())

if "--list" in sys.argv:
    print(json.dumps(data, indent=2))
elif "--host" in sys.argv:
    host = sys.argv[sys.argv.index("--host") + 1]
    hostvars = data.get("_meta", {}).get("hostvars", {})
    print(json.dumps(hostvars.get(host, {}), indent=2))
