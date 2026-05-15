# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Anthony Green <green@moxielogic.com>

"""
DirQ connection plugin for Ansible / AAP.

Routes exec_command(), put_file(), and fetch_file() through the DirQ
server REST API and relay mesh. No SSH or WinRM required.

Usage in a playbook:

    - hosts: all
      connection: atgreen.dirq.dirq
"""

from __future__ import annotations

import base64
import os

from ansible.errors import AnsibleConnectionFailure, AnsibleError
from ansible.plugins.connection import ConnectionBase

DOCUMENTATION = """
    name: atgreen.dirq.dirq
    short_description: Connect via DirQ relay mesh
    description:
        - Executes commands and transfers files through the DirQ server
          and P2P relay mesh. No SSH or WinRM required.
        - The target host must have a DirQ agent with exec_enabled=true.
    author: Anthony Green
    options:
        dirq_server_url:
            description: URL of the DirQ server REST API.
            default: http://localhost:8080
            type: str
            vars:
                - name: dirq_server_url
            env:
                - name: DIRQ_SERVER_URL
        dirq_token:
            description: DirQ API token for authentication.
            default: ""
            type: str
            vars:
                - name: dirq_token
            env:
                - name: DIRQ_TOKEN
        dirq_exec_timeout:
            description: Timeout in seconds for exec operations.
            default: 300
            type: int
            vars:
                - name: dirq_exec_timeout
        dirq_file_timeout:
            description: Timeout in seconds for file transfer operations.
            default: 300
            type: int
            vars:
                - name: dirq_file_timeout
"""


class Connection(ConnectionBase):
    """DirQ connection plugin for AAP."""

    transport = "atgreen.dirq.dirq"
    has_pipelining = False

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._connected = False
        self._agent_id = None
        self._client = None

    def _connect(self):
        if self._connected:
            return self

        host = self._play_context.remote_addr

        # Host variables set by the inventory plugin (dirq_agent_id,
        # dirq_server_url) are available through the play context.
        hostvars = getattr(self._play_context, "vars", None) or {}

        server_url = (
            hostvars.get("dirq_server_url")
            or os.environ.get("DIRQ_SERVER_URL")
            or self.get_option("dirq_server_url")
            or ""
        )
        token = (
            os.environ.get("DIRQ_TOKEN")
            or self.get_option("dirq_token")
            or ""
        )

        if not server_url:
            raise AnsibleConnectionFailure(
                "DIRQ_SERVER_URL is not set. Set it in the environment, "
                "or use the DirQ inventory plugin which sets dirq_server_url per host."
            )

        from ansible_collections.atgreen.dirq.plugins.module_utils.api import DirQClient
        self._client = DirQClient(server_url, token)

        # Route by stable dirq_agent_id (set by inventory plugin), not hostname.
        self._agent_id = hostvars.get("dirq_agent_id")

        if not self._agent_id:
            # Fallback: resolve by hostname if not using the DirQ inventory plugin.
            self._agent_id = self._resolve_agent_id_by_hostname(host)

        if not self._agent_id:
            raise AnsibleConnectionFailure(
                f"Could not resolve DirQ agent_id for host '{host}'. "
                "Ensure the host is registered in DirQ and online."
            )

        self._display.vvv(
            f"DIRQ: connected to {host} (agent_id={self._agent_id})",
            host=host,
        )
        self._connected = True
        return self

    def _resolve_agent_id_by_hostname(self, hostname):
        """Fallback: scan hosts API to find agent by hostname."""
        try:
            hosts = self._client.get("/api/v1/hosts")
            for host in hosts:
                if host.get("hostname") == hostname and host.get("online"):
                    return host.get("id")
        except Exception as e:
            raise AnsibleConnectionFailure(
                f"Failed to look up agent for '{hostname}': {e}"
            )
        return None

    def exec_command(self, cmd, in_data=None, sudoable=True):
        self._connect()

        become = self._play_context.become
        become_user = self._play_context.become_user or "root"
        become_method = self._play_context.become_method or "sudo"

        timeout = self.get_option("dirq_exec_timeout") or 300

        payload = {
            "agent_id": self._agent_id,
            "command": cmd,
            "become": become and sudoable,
            "become_user": become_user,
            "become_method": become_method,
            "timeout": timeout,
        }

        # Pass stdin data — required for Ansible modules which pipe Python
        # module code to the target via stdin.
        if in_data:
            payload["stdin"] = base64.b64encode(in_data).decode("ascii")

        # AAP attribution from EE environment.
        job_id = os.environ.get("AWX_JOB_ID", os.environ.get("AAP_JOB_ID", ""))
        if job_id:
            payload["aap_job_id"] = job_id
            payload["aap_job_template"] = os.environ.get("AWX_JOB_TEMPLATE_NAME", "")
            payload["aap_user"] = os.environ.get("AWX_USER_NAME", "")

        self._display.vvv(f"DIRQ exec: {cmd}", host=self._play_context.remote_addr)

        try:
            result = self._client.post("/api/v1/exec", payload)
        except Exception as e:
            raise AnsibleConnectionFailure(f"DirQ exec failed: {e}")

        rc = result.get("rc", -1)
        stdout_b64 = result.get("stdout", "")
        stderr_b64 = result.get("stderr", "")

        stdout = base64.b64decode(stdout_b64) if stdout_b64 else b""
        stderr = base64.b64decode(stderr_b64) if stderr_b64 else b""

        if result.get("error") and not result.get("success"):
            stderr = result["error"].encode("utf-8") + b"\n" + stderr

        return rc, stdout, stderr

    def put_file(self, in_path, out_path):
        self._connect()

        with open(in_path, "rb") as f:
            content = base64.b64encode(f.read()).decode("ascii")

        try:
            mode = os.stat(in_path).st_mode & 0o7777
        except OSError:
            mode = 0o644

        timeout = self.get_option("dirq_file_timeout") or 300

        payload = {
            "agent_id": self._agent_id,
            "dest_path": out_path,
            "content": content,
            "mode": mode,
            "become": self._play_context.become,
            "become_user": self._play_context.become_user or "root",
            "timeout": timeout,
        }

        job_id = os.environ.get("AWX_JOB_ID", os.environ.get("AAP_JOB_ID", ""))
        if job_id:
            payload["aap_job_id"] = job_id
            payload["aap_job_template"] = os.environ.get("AWX_JOB_TEMPLATE_NAME", "")
            payload["aap_user"] = os.environ.get("AWX_USER_NAME", "")

        self._display.vvv(
            f"DIRQ put_file: {in_path} -> {out_path}",
            host=self._play_context.remote_addr,
        )

        try:
            result = self._client.post("/api/v1/put_file", payload)
        except Exception as e:
            raise AnsibleError(f"DirQ put_file failed: {e}")

        if not result.get("success"):
            raise AnsibleError(
                f"DirQ put_file failed: {result.get('error', 'unknown error')}"
            )

    def fetch_file(self, in_path, out_path):
        self._connect()

        timeout = self.get_option("dirq_file_timeout") or 300

        payload = {
            "agent_id": self._agent_id,
            "src_path": in_path,
            "become": self._play_context.become,
            "become_user": self._play_context.become_user or "root",
            "timeout": timeout,
        }

        job_id = os.environ.get("AWX_JOB_ID", os.environ.get("AAP_JOB_ID", ""))
        if job_id:
            payload["aap_job_id"] = job_id
            payload["aap_job_template"] = os.environ.get("AWX_JOB_TEMPLATE_NAME", "")
            payload["aap_user"] = os.environ.get("AWX_USER_NAME", "")

        self._display.vvv(
            f"DIRQ fetch_file: {in_path} -> {out_path}",
            host=self._play_context.remote_addr,
        )

        try:
            result = self._client.post("/api/v1/fetch_file", payload)
        except Exception as e:
            raise AnsibleError(f"DirQ fetch_file failed: {e}")

        if not result.get("success"):
            raise AnsibleError(
                f"DirQ fetch_file failed: {result.get('error', 'unknown error')}"
            )

        content = base64.b64decode(result.get("content", ""))
        with open(out_path, "wb") as f:
            f.write(content)

    def close(self):
        self._connected = False
