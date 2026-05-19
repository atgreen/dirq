# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Anthony Green <green@moxielogic.com>

"""Shared HTTP client for DirQ server API."""

from __future__ import annotations

import json
import os
import random
import ssl
import time
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


class DirQClient:
    """Simple HTTP client for the DirQ server REST API."""

    def __init__(self, server_url: str, token: str = "", timeout: int = 600,
                 tls_insecure: bool | None = None):
        self.server_url = server_url.rstrip("/")
        self.token = token
        self.timeout = timeout

        # Build SSL context for HTTPS requests.
        self._ssl_context = None
        if self.server_url.startswith("https://"):
            insecure = tls_insecure
            if insecure is None:
                insecure = os.environ.get("DIRQ_TLS_INSECURE", "").lower() == "true"
            if insecure:
                self._ssl_context = ssl.create_default_context()
                self._ssl_context.check_hostname = False
                self._ssl_context.verify_mode = ssl.CERT_NONE

    def request(self, method: str, path: str, data: dict | None = None) -> dict | list:
        url = self.server_url + path
        body = None
        if data is not None:
            body = json.dumps(data).encode("utf-8")

        # Retry on 429 with exponential backoff + jitter. Ansible at high
        # --forks values can briefly exceed the server's per-token rate limit.
        max_attempts = 6
        for attempt in range(max_attempts):
            req = Request(url, data=body, method=method)
            req.add_header("Content-Type", "application/json")
            if self.token:
                req.add_header("Authorization", f"Bearer {self.token}")

            try:
                resp = urlopen(req, timeout=self.timeout, context=self._ssl_context)
                resp_data = resp.read().decode("utf-8")
                return json.loads(resp_data) if resp_data else {}
            except HTTPError as e:
                if e.code == 429 and attempt < max_attempts - 1:
                    sleep_for = min(2 ** attempt, 8) * (0.5 + random.random())
                    time.sleep(sleep_for)
                    continue
                raise RuntimeError(f"DirQ API request failed: {method} {url}: {e}") from e
            except URLError as e:
                raise RuntimeError(f"DirQ API request failed: {method} {url}: {e}") from e

    def get(self, path: str) -> dict | list:
        return self.request("GET", path)

    def post(self, path: str, data: dict) -> dict:
        return self.request("POST", path, data)
