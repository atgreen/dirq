# CLI

**Config file:** `~/.config/dirq/client.conf` (user-local, checked first) or `/etc/dirq/client.conf` (system-wide). On Windows: `%APPDATA%\dirq\client.conf` or `C:\ProgramData\dirq\client.conf`. The server generates a ready-to-copy `client.conf` at `/var/lib/dirq/client.conf`.

```
# ~/.config/dirq/client.conf
server_url: https://dirq-server:8080
token: <your-api-token>
tls_ca: /etc/dirq/ca.crt
```

`tls_ca` points at the CA that signed the server's certificate — the one
`dirq cert generate` produces, or your own. The server writes a
ready-to-copy `client.conf` containing the right path. Copy the CA file to
the client machine alongside it.

Leave `tls_ca` out only if the server's certificate is signed by a CA your
system already trusts.

| Config key | Variable / Flag | Default | Description |
|-----------|----------------|---------|-------------|
| `server_url` | `DIRQ_SERVER_URL` / `--server` | *(required)* | Server REST URL |
| `token` | `DIRQ_TOKEN` / `--token` | | API token |
| `tls_ca` | `DIRQ_TLS_CA` / `--tls-ca` | | CA certificate used to verify the server. Same variable the server and agents use, so one value works for all three |
| `tls_insecure` | `DIRQ_TLS_INSECURE` / `--tls-insecure` | `false` | Accept any server certificate. Prefer `tls_ca`; if both are set the CA wins |
| | `--config` / `DIRQ_CONFIG_FILE` | *(see above)* | Read a specific config file instead of the default locations |
| `llm_url` | `DIRQ_LLM_URL` | | LLM API base URL (Anthropic or OpenAI-compatible) |
| `llm_api_key` | `DIRQ_LLM_API_KEY` | | LLM API key |
| `llm_model` | `DIRQ_LLM_MODEL` | `claude-sonnet-4-20250514` | LLM model name |
| | `--json` | `false` | Raw JSON output |

For `dirq ask`, if `DIRQ_LLM_*` is not configured, falls back to `ANTHROPIC_API_KEY` with Anthropic's native API.
