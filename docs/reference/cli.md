# CLI

**Config file:** `~/.config/dirq/client.conf` (user-local, checked first) or `/etc/dirq/client.conf` (system-wide). On Windows: `%APPDATA%\dirq\client.conf` or `C:\ProgramData\dirq\client.conf`. The server generates a ready-to-copy `client.conf` at `/var/lib/dirq/client.conf`.

```
# ~/.config/dirq/client.conf
server_url: https://dirq-server:8080
token: <your-api-token>
tls_insecure: true
```

| Config key | Variable / Flag | Default | Description |
|-----------|----------------|---------|-------------|
| `server_url` | `DIRQ_SERVER_URL` / `--server` | *(required)* | Server REST URL |
| `token` | `DIRQ_TOKEN` / `--token` | | API token |
| `tls_insecure` | `DIRQ_TLS_INSECURE` / `--tls-insecure` | `false` | Skip TLS verification |
| `llm_url` | `DIRQ_LLM_URL` | | LLM API base URL (Anthropic or OpenAI-compatible) |
| `llm_api_key` | `DIRQ_LLM_API_KEY` | | LLM API key |
| `llm_model` | `DIRQ_LLM_MODEL` | `claude-sonnet-4-20250514` | LLM model name |
| | `--json` | `false` | Raw JSON output |

For `dirq ask`, if `DIRQ_LLM_*` is not configured, falls back to `ANTHROPIC_API_KEY` with Anthropic's native API.
