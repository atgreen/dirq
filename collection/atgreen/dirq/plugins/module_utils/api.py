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
                 tls_insecure: bool | None = None, tls_ca: str | None = None):
        self.server_url = server_url.rstrip("/")
        self.token = token
        self.timeout = timeout
        self._ssl_context = self._build_ssl_context(tls_insecure, tls_ca)

    def _build_ssl_context(self, tls_insecure, tls_ca):
        """Decide how the server certificate is verified.

        DIRQ_TLS_CA names a CA to verify against - the same variable the
        server, the agents and the dirq CLI all read. It matters because
        `dirq cert generate` produces a self-signed CA that is in no system
        trust store, so without this option the only working configuration
        against a default deployment was to turn verification off for every
        request. These plugins carry the DirQ API token on each one
        (dirq-632.6).

        A CA takes precedence over DIRQ_TLS_INSECURE: naming one is the more
        specific instruction. A CA path that cannot be read is fatal rather
        than a quiet fall back to the system trust store, which would verify
        against something other than what was asked for and look like it
        worked.
        """
        if not self.server_url.startswith("https://"):
            return None

        ca = tls_ca
        if ca is None:
            ca = os.environ.get("DIRQ_TLS_CA", "")
        ca = (ca or "").strip()
        if ca:
            return ssl.create_default_context(cafile=ca)

        insecure = tls_insecure
        if insecure is None:
            insecure = os.environ.get("DIRQ_TLS_INSECURE", "").lower() == "true"
        if insecure:
            ctx = ssl.create_default_context()
            ctx.check_hostname = False
            ctx.verify_mode = ssl.CERT_NONE
            return ctx

        return None

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
