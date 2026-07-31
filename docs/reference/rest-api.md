# REST API

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/query` | Submit a DirQ query |
| `GET` | `/api/v1/hosts` | List hosts |
| `GET` | `/api/v1/hosts/{id}` | Host details |
| `GET` | `/api/v1/hosts/{id}/facts` | Cached facts |
| `PUT` | `/api/v1/hosts/{id}/tags` | Replace tags |
| `PATCH` | `/api/v1/hosts/{id}/tags` | Merge tags |
| `DELETE` | `/api/v1/hosts/{id}/tags/{key}` | Remove tag |
| `GET` | `/api/v1/queries` | Recent queries |
| `POST` | `/api/v1/tokens` | Create token |
| `GET` | `/api/v1/tokens` | List tokens |
| `DELETE` | `/api/v1/tokens/{name}` | Delete token |
| `GET` | `/api/v1/inventory` | Ansible inventory |
| `POST` | `/api/v1/exec` | Execute command (single agent) |
| `POST` | `/api/v1/exec_multi` | Execute command/script across fleet (streaming NDJSON) |
| `POST` | `/api/v1/put_file` | Write file |
| `POST` | `/api/v1/fetch_file` | Read file |
| `GET` | `/api/v1/exec_log` | Exec audit log |
| `GET` | `/api/v1/debug/inflight` | In-flight broadcast sessions with per-ZL breakdown (admin) |
| `GET` | `/api/v1/status` | Fleet status (agent counts, ZLs, tree depth, database kind) |
| `GET` | `/healthz` | Liveness — process is up |
| `GET` | `/readyz` | Readiness — this pod is the active leader (200) or a standby (503); always 200 when leader election is disabled |
| `GET` | `/metrics` | Prometheus scrape (unauth; see [Observability](observability.md)) |
