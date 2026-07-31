# Metrics & observability

The server exposes a Prometheus scrape endpoint at `/metrics` (unauthenticated; restrict at the network layer if needed). Two families:

**dirq self-health** — counts, durations, gauges describing the server's own behavior:

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `dirq_build_info` | gauge=1 | `version,backend,go_version` | Pin running build |
| `dirq_agents_total` / `dirq_agents_online` | gauge | — | Fleet size |
| `dirq_zone_leaders` | gauge | — | Direct server connections |
| `dirq_tree_depth_max` | gauge | — | Deepest path in the relay tree |
| `dirq_subtree_size` | gauge | `zone_leader` (hostname) | Per-ZL fan-out — spot imbalance |
| `dirq_agents_on_probation` | gauge | — | Agents currently kept near the leaves for rebooting ([Reboot-Aware Placement](../explanation/reboot-aware-placement.md)) |
| `dirq_failure_domains_hot` | gauge | — | Subnets with a correlated reboot underway |
| `dirq_orphan_reassign_total` | counter | `action` (reparent/promote) | Reactive re-homing after a parent dropped |
| `dirq_inflight_sessions` | gauge | `kind` (query/exec/deploy) | Active broadcasts |
| `dirq_inflight_pending_targets` | gauge | `kind` | Sum of unaccounted targets — is anything stuck? |
| `dirq_broadcast_total` | counter | `kind,outcome` (complete/incomplete/hard_timeout/canceled) | Activity + reliability |
| `dirq_broadcast_duration_seconds` | histogram | `kind` | Latency |
| `dirq_broadcast_missing_total` | counter | `kind` | Sum of did-not-reply across completions |
| `dirq_register_total` | counter | `outcome` (ok/rejected_secret/rejected_other) | Registration activity |
| `dirq_register_duration_seconds` | histogram | — | Register RPC latency |
| `dirq_peer_disconnect_total` / `dirq_peer_connect_total` | counter | — | Mesh churn |
| `dirq_fact_stage_depth` | gauge | — | Fact-cache backpressure |
| `dirq_fact_flush_total` | counter | `backend,outcome` | Postgres/SQLite write activity |
| `dirq_fact_flush_duration_seconds` | histogram | `backend` | SQLite writer-lock watch |
| `dirq_server_cert_expiry_seconds` | gauge | — | Server TLS cert countdown (alert if < 7d) |

Plus all free Go runtime metrics (`go_goroutines`, `go_memstats_*`, `go_gc_duration_seconds`, etc.).

**Fleet composition** — aggregated views of the managed fleet, sliced by collected facts. One combined gauge with bounded-cardinality labels:

```
dirq_fleet_count{os,distro,distro_version,arch,cores_bucket,memory_gb_bucket,exec_enabled,online}
```

Major distro version only (`8` not `8.10`) to bound cardinality; minor versions remain queryable via the Postgres data source (below). Recomputed every 30 s (`refreshFleetMetricsInterval`) so /metrics scrapes stay cheap.

## Sample PromQL

```promql
# Fleet count by distro + major version, stacked area
sum by (distro, distro_version) (dirq_fleet_count{online="true"})

# Online percentage trend
dirq_agents_online / dirq_agents_total

# Did-not-reply rate, last 5 min
rate(dirq_broadcast_missing_total[5m])
  / rate(dirq_broadcast_total{outcome=~"complete|incomplete"}[5m])

# 95p exec duration
histogram_quantile(0.95, rate(dirq_broadcast_duration_seconds_bucket{kind="exec"}[5m]))

# Cert expiry alert
dirq_server_cert_expiry_seconds < 7 * 86400
```

## Prometheus scrape config

```yaml
scrape_configs:
  - job_name: dirq
    metrics_path: /metrics
    scheme: https            # drop to http if TLS is disabled
    tls_config:
      insecure_skip_verify: true   # if using self-signed certs
    static_configs:
      - targets: ['dirq-server:8080']
```

Default retention (15 d) is enough for week-over-week trends; bump `--storage.tsdb.retention.time=90d` for quarterly views.

## Grafana — Postgres data source for ad-hoc panels

For queries the Prometheus metrics don't cover (per-host kernel versions, specific package presence, disk usage above N%), point Grafana at the dirq database directly with a read-only role:

```sql
CREATE ROLE grafana_readonly LOGIN PASSWORD '...';
GRANT CONNECT ON DATABASE dirq TO grafana_readonly;
GRANT USAGE ON SCHEMA public TO grafana_readonly;
GRANT SELECT ON agents, agent_facts, exec_log, queries TO grafana_readonly;
```

Then panels are SQL against the `agents` and `agent_facts` tables:

```sql
-- Top 20 hosts by disk usage in prod
SELECT a.hostname,
       p->>'mount_point' AS mount,
       (p->>'pct_used')::float AS pct_used
FROM agents a
JOIN agent_facts f ON f.agent_id = a.id AND f.module = 'disk'
CROSS JOIN LATERAL jsonb_array_elements(f.data->'partitions') AS p
WHERE a.tags->>'env' = 'prod'
  AND (p->>'pct_used')::float > 85
ORDER BY pct_used DESC
LIMIT 20;

-- Distinct kernel versions present today
SELECT data->>'kernel_version' AS kernel, COUNT(*) AS hosts
FROM agent_facts
WHERE module = 'os_info'
GROUP BY 1
ORDER BY 2 DESC;
```

Postgres queries return current state only — for time-series trends use the Prometheus metrics. For retention beyond what Prometheus holds, an external snapshot table is the standard option but isn't bundled.

For interpreting these signals during an incident, see the [diagnostics how-to](../how-to/diagnostics.md).
