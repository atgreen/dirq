"""
DirQ connection plugin for Ansible / AAP.

Routes exec_command(), put_file(), and fetch_file() through the DirQ
server REST API and relay mesh to reach managed hosts without SSH/WinRM.

Usage in a playbook:
    - hosts: all
      connection: dirq
      vars:
        dirq_server_url: http://dirq-server:8080
        dirq_token: your-api-token

Or set environment variables:
    DIRQ_SERVER_URL=http://dirq-server:8080
    DIRQ_TOKEN=your-api-token
"""

from __future__ import annotations

import base64
import json
import os
import shlex
import tempfile
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
            vars:
                - name: dirq_token
            env:
                - name: DIRQ_TOKEN
        dirq_exec_timeout:
            description: Timeout in seconds for exec operations.
            default: 60
            vars:
                - name: dirq_exec_timeout
        dirq_file_timeout:
            description: Timeout in seconds for file transfer operations.
            default: 300
            vars:
                - name: dirq_file_timeout
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
        """Establish the connection — resolve hostname to agent_id."""
        if self._connected:
            return self

        self._server_url = (
            self.get_option("dirq_server_url")
            or os.environ.get("DIRQ_SERVER_URL", "http://localhost:8080")
        )
        self._token = (
            self.get_option("dirq_token")
            or os.environ.get("DIRQ_TOKEN", "")
        )

        # Resolve the inventory hostname to a DirQ agent_id.
        # The inventory plugin sets dirq_agent_id as a hostvar.
        host_vars = self._play_context._attributes.get("vars", {}) if hasattr(self._play_context, '_attributes') else {}
        self._agent_id = self._get_agent_id()

        if not self._agent_id:
            raise AnsibleConnectionFailure(
                f"Could not resolve DirQ agent_id for host '{self._play_context.remote_addr}'. "
                "Ensure the host is registered in DirQ and the inventory plugin sets dirq_agent_id."
            )

        self._connected = True
        return self

    def _get_agent_id(self):
        """Look up agent_id from hostvars or by querying the server."""
        # Try hostvars first (set by DirQ inventory plugin).
        try:
            agent_id = self._play_context.remote_addr
            # If it looks like a UUID, use it directly.
            if len(agent_id) > 30 and "-" in agent_id:
                return agent_id
        except Exception:
            pass

        # Otherwise, look up by hostname via the server API.
        hostname = self._play_context.remote_addr
        try:
            hosts = self._api_request("GET", "/api/v1/hosts")
            for host in hosts:
                if host.get("hostname") == hostname:
                    return host.get("id")
        except Exception as e:
            raise AnsibleConnectionFailure(f"Failed to look up agent for '{hostname}': {e}")

        return None

    def exec_command(self, cmd, in_data=None, sudoable=True):
        """Execute a command on the remote host via DirQ."""
        self._connect()

        become = self._play_context.become
        become_user = self._play_context.become_user or "root"
        become_method = self._play_context.become_method or "sudo"

        timeout = int(
            self.get_option("dirq_exec_timeout")
            or os.environ.get("DIRQ_EXEC_TIMEOUT", "60")
        )

        payload = {
            "agent_id": self._agent_id,
            "command": cmd,
            "become": become and sudoable,
            "become_user": become_user,
            "become_method": become_method,
            "timeout": timeout,
        }

        # Add AAP attribution if available.
        job_id = os.environ.get("AWX_JOB_ID", os.environ.get("AAP_JOB_ID", ""))
        if job_id:
            payload["aap_job_id"] = job_id
            payload["aap_job_template"] = os.environ.get("AWX_JOB_TEMPLATE_NAME", "")
            payload["aap_user"] = os.environ.get("AWX_USER_NAME", "")

        try:
            result = self._api_request("POST", "/api/v1/exec", payload)
        except Exception as e:
            raise AnsibleConnectionFailure(f"DirQ exec failed: {e}")

        rc = result.get("rc", -1)
        stdout = result.get("stdout", "")
        stderr = result.get("stderr", "")

        if result.get("error"):
            stderr = result["error"] + "\n" + stderr

        return rc, stdout.encode("utf-8"), stderr.encode("utf-8")

    def put_file(self, in_path, out_path):
        """Transfer a file to the remote host via DirQ."""
        self._connect()

        with open(in_path, "rb") as f:
            content = base64.b64encode(f.read()).decode("ascii")

        # Get file mode.
        try:
            mode = os.stat(in_path).st_mode & 0o7777
        except OSError:
            mode = 0o644

        become = self._play_context.become
        become_user = self._play_context.become_user or "root"

        timeout = int(
            self.get_option("dirq_file_timeout")
            or os.environ.get("DIRQ_FILE_TIMEOUT", "300")
        )

        payload = {
            "agent_id": self._agent_id,
            "dest_path": out_path,
            "content": content,
            "mode": mode,
            "become": become,
            "become_user": become_user,
            "timeout": timeout,
        }

        job_id = os.environ.get("AWX_JOB_ID", os.environ.get("AAP_JOB_ID", ""))
        if job_id:
            payload["aap_job_id"] = job_id
            payload["aap_job_template"] = os.environ.get("AWX_JOB_TEMPLATE_NAME", "")
            payload["aap_user"] = os.environ.get("AWX_USER_NAME", "")

        try:
            result = self._api_request("POST", "/api/v1/put_file", payload)
        except Exception as e:
            raise AnsibleError(f"DirQ put_file failed: {e}")

        if not result.get("success"):
            raise AnsibleError(f"DirQ put_file failed: {result.get('error', 'unknown error')}")

    def fetch_file(self, in_path, out_path):
        """Fetch a file from the remote host via DirQ."""
        self._connect()

        become = self._play_context.become
        become_user = self._play_context.become_user or "root"

        timeout = int(
            self.get_option("dirq_file_timeout")
            or os.environ.get("DIRQ_FILE_TIMEOUT", "300")
        )

        payload = {
            "agent_id": self._agent_id,
            "src_path": in_path,
            "become": become,
            "become_user": become_user,
            "timeout": timeout,
        }

        job_id = os.environ.get("AWX_JOB_ID", os.environ.get("AAP_JOB_ID", ""))
        if job_id:
            payload["aap_job_id"] = job_id
            payload["aap_job_template"] = os.environ.get("AWX_JOB_TEMPLATE_NAME", "")
            payload["aap_user"] = os.environ.get("AWX_USER_NAME", "")

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
        """Close the connection."""
        self._connected = False

    # ─────────────────────────────────────────────────────
    # HTTP helpers
    # ─────────────────────────────────────────────────────

    def _api_request(self, method, path, data=None):
        """Make an HTTP request to the DirQ server REST API."""
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
