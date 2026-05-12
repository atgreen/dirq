# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Anthony Green <green@moxielogic.com>

"""Shared HTTP client for DirQ server API."""

from __future__ import annotations

import json
from urllib.error import URLError
from urllib.request import Request, urlopen


class DirQClient:
    """Simple HTTP client for the DirQ server REST API."""

    def __init__(self, server_url: str, token: str = "", timeout: int = 600):
        self.server_url = server_url.rstrip("/")
        self.token = token
        self.timeout = timeout

    def request(self, method: str, path: str, data: dict | None = None) -> dict | list:
        url = self.server_url + path
        body = None
        if data is not None:
            body = json.dumps(data).encode("utf-8")

        req = Request(url, data=body, method=method)
        req.add_header("Content-Type", "application/json")
        if self.token:
            req.add_header("Authorization", f"Bearer {self.token}")

        try:
            resp = urlopen(req, timeout=self.timeout)
            resp_data = resp.read().decode("utf-8")
            return json.loads(resp_data) if resp_data else {}
        except URLError as e:
            raise RuntimeError(f"DirQ API request failed: {method} {url}: {e}") from e

    def get(self, path: str) -> dict | list:
        return self.request("GET", path)

    def post(self, path: str, data: dict) -> dict:
        return self.request("POST", path, data)
