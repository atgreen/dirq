"""
DirQ connection plugin for Ansible / AAP.

Routes exec_command(), put_file(), and fetch_file() through the DirQ
server REST API and relay mesh to reach managed hosts without SSH/WinRM.
"""

from __future__ import annotations

import base64
import json
import os
from urllib.error import URLError
from urllib.request import Request, urlopen

from ansible.errors import AnsibleConnectionFailure, AnsibleError
from ansible.plugins.connection import ConnectionBase

DOCUMENTATION = """
    name: dirq
    short_description: Connect via DirQ relay mesh
    description:
        - Executes commands and transfers files through the DirQ server
          and P2P relay mesh. No SSH or WinRM required.
        - The target host must have a DirQ agent with exec_enabled=true.
    author: DirQ Project
    options:
        dirq_server_url:
            description: URL of the DirQ server REST API.
            default: http://localhost:8080
            vars:
                - name: dirq_server_url
            env:
                - name: DIRQ_SERVER_URL
        dirq_token:
            description: DirQ API token for authentication.
            default: ""
            vars:
                - name: dirq_token
            env:
                - name: DIRQ_TOKEN
"""


class Connection(ConnectionBase):
    """DirQ connection plugin."""

    transport = "dirq"
    has_pipelining = False

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._connected = False
        self._agent_id = None
        self._server_url = None
        self._token = None

    def _connect(self):
        if self._connected:
            return self

        self._server_url = (
            os.environ.get("DIRQ_SERVER_URL")
            or self.get_option("dirq_server_url")
            or "http://localhost:8080"
        )
        self._token = (
            os.environ.get("DIRQ_TOKEN")
            or self.get_option("dirq_token")
            or ""
        )

        # Resolve hostname to agent_id by querying the server.
        hostname = self._play_context.remote_addr
        self._agent_id = self._resolve_agent_id(hostname)

        if not self._agent_id:
            raise AnsibleConnectionFailure(
                f"Could not resolve DirQ agent_id for host '{hostname}'. "
                "Ensure the host is registered in DirQ with exec_enabled=true."
            )

        self._display.vvv(f"DIRQ: connected to {hostname} (agent_id={self._agent_id})", host=hostname)
        self._connected = True
        return self

    def _resolve_agent_id(self, hostname):
        """Look up agent_id by hostname via the server API."""
        try:
            hosts = self._api_request("GET", "/api/v1/hosts")
            for host in hosts:
                if host.get("hostname") == hostname and host.get("online"):
                    return host.get("id")
        except Exception as e:
            raise AnsibleConnectionFailure(f"Failed to look up agent for '{hostname}': {e}")
        return None

    def exec_command(self, cmd, in_data=None, sudoable=True):
        self._connect()

        become = self._play_context.become
        become_user = self._play_context.become_user or "root"
        become_method = self._play_context.become_method or "sudo"

        payload = {
            "agent_id": self._agent_id,
            "command": cmd,
            "become": become and sudoable,
            "become_user": become_user,
            "become_method": become_method,
            "timeout": 60,
        }

        # AAP attribution.
        job_id = os.environ.get("AWX_JOB_ID", os.environ.get("AAP_JOB_ID", ""))
        if job_id:
            payload["aap_job_id"] = job_id
            payload["aap_job_template"] = os.environ.get("AWX_JOB_TEMPLATE_NAME", "")
            payload["aap_user"] = os.environ.get("AWX_USER_NAME", "")

        self._display.vvv(f"DIRQ exec: {cmd}", host=self._play_context.remote_addr)

        try:
            result = self._api_request("POST", "/api/v1/exec", payload)
        except Exception as e:
            raise AnsibleConnectionFailure(f"DirQ exec failed: {e}")

        rc = result.get("rc", -1)
        stdout = result.get("stdout", "")
        stderr = result.get("stderr", "")

        if result.get("error") and not result.get("success"):
            stderr = result["error"] + "\n" + stderr

        return rc, stdout.encode("utf-8"), stderr.encode("utf-8")

    def put_file(self, in_path, out_path):
        self._connect()

        with open(in_path, "rb") as f:
            content = base64.b64encode(f.read()).decode("ascii")

        try:
            mode = os.stat(in_path).st_mode & 0o7777
        except OSError:
            mode = 0o644

        payload = {
            "agent_id": self._agent_id,
            "dest_path": out_path,
            "content": content,
            "mode": mode,
            "become": self._play_context.become,
            "become_user": self._play_context.become_user or "root",
            "timeout": 300,
        }

        self._display.vvv(f"DIRQ put_file: {in_path} -> {out_path}", host=self._play_context.remote_addr)

        try:
            result = self._api_request("POST", "/api/v1/put_file", payload)
        except Exception as e:
            raise AnsibleError(f"DirQ put_file failed: {e}")

        if not result.get("success"):
            raise AnsibleError(f"DirQ put_file failed: {result.get('error', 'unknown error')}")

    def fetch_file(self, in_path, out_path):
        self._connect()

        payload = {
            "agent_id": self._agent_id,
            "src_path": in_path,
            "become": self._play_context.become,
            "become_user": self._play_context.become_user or "root",
            "timeout": 300,
        }

        self._display.vvv(f"DIRQ fetch_file: {in_path} -> {out_path}", host=self._play_context.remote_addr)

        try:
            result = self._api_request("POST", "/api/v1/fetch_file", payload)
        except Exception as e:
            raise AnsibleError(f"DirQ fetch_file failed: {e}")

        if not result.get("success"):
            raise AnsibleError(f"DirQ fetch_file failed: {result.get('error', 'unknown error')}")

        content = base64.b64decode(result.get("content", ""))
        with open(out_path, "wb") as f:
            f.write(content)

    def close(self):
        self._connected = False

    def _api_request(self, method, path, data=None):
        url = self._server_url.rstrip("/") + path

        body = None
        if data is not None:
            body = json.dumps(data).encode("utf-8")

        req = Request(url, data=body, method=method)
        req.add_header("Content-Type", "application/json")
        if self._token:
            req.add_header("Authorization", f"Bearer {self._token}")

        try:
            resp = urlopen(req, timeout=600)
            resp_data = resp.read().decode("utf-8")
            return json.loads(resp_data) if resp_data else {}
        except URLError as e:
            raise AnsibleConnectionFailure(
                f"DirQ server request failed: {method} {url}: {e}"
            )
