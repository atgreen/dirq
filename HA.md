# DirQ High Availability

This document describes how to run the DirQ server in a highly
available configuration on Kubernetes / OpenShift. It covers the
deployment model, the leader-election mechanism, failover behavior,
and the platform-side wiring required to make it work.

## TL;DR

- Run **N pods** of `dirq-server` against a shared **PostgreSQL** database.
- Set `DIRQ_LEADER_ELECTION=true` on every pod.
- Use **`/readyz`** as the readiness probe.
- Point your `Service` / `Route` at the pods using the standard
  selector — Kubernetes will keep only the pod that holds the lock in
  the Service's endpoint set.

That's it. There is no cross-pod request routing, no per-agent
stickiness, no gossip layer. The leader serves; the standbys wait.

## Why active/standby (and not active/active)

The DirQ server doesn't hold a connection per managed host — it only
holds gRPC streams from **zone leaders**. A fleet of 10 000 hosts
typically yields a few dozen direct connections at the server. The
server is not connection-bound; it has no load problem that
spreading across pods would solve.

What it does have is a **single point of failure** when run as one
pod: if the server pod dies, the entire mesh loses its root and
every agent eventually times out. The goal of HA in DirQ is
**survivability**, not throughput.

Active/standby fits the workload exactly. The leader holds the streams
and serves the REST API. Standbys are warm (connected to the DB,
configuration loaded, listening on gRPC/HTTP) but report
`/readyz` → 503 so Kubernetes excludes them from the Service's endpoint
list. On leader death, the standby promotes itself within seconds,
becomes Ready, and the LB switches over. Agents notice the broken
stream and reconnect — landing on the new leader through the same
external endpoint.

Active/active would require cross-pod request forwarding (an exec
landing on pod B for an agent connected to pod A would have to be
forwarded over an internal mesh). That's a meaningful chunk of code
to write and operate, and the workload doesn't justify it. We didn't
build it.

## Leader election mechanism

Leader election uses a PostgreSQL **session-level advisory lock**.
SQLite deployments are single-instance by construction; the SQLite
backend exposes a no-op leader that always reports leadership.

### Lock semantics

```sql
SELECT pg_try_advisory_lock(0x4449525100000001);
```

- The lock key (`0x4449525100000001` — "DIRQ\0\0\0\1") is fixed across
  DirQ versions. Never change it.
- The lock is **session-level**: it is automatically released when the
  Postgres connection holding it terminates, whether by clean exit or
  by network drop. There is no separate keep-alive to manage.
- Only one Postgres session can hold the lock at a time. Other sessions
  calling `pg_try_advisory_lock` get `false` immediately (non-blocking).

### Worker behavior

Each pod runs a goroutine that, every 2 seconds:

- If currently leader: `Ping()` the dedicated lock-holding connection.
  If the ping fails, demote — Postgres has already released the lock,
  so another pod will pick it up on its next poll.
- If currently standby: try `pg_try_advisory_lock`. If it returns
  `true`, promote.

The dedicated `*pgxpool.Conn` is held out of the pool for the entire
duration of leadership. On graceful shutdown the worker explicitly calls
`pg_advisory_unlock` before releasing the connection so a standby can
promote within one poll interval instead of waiting for Postgres to
detect the TCP close (which can take ~30 seconds depending on tuning).

### Configuration

| Knob | Default | Description |
|---|---|---|
| `DIRQ_LEADER_ELECTION` env / `leader_election:` config | `false` | Enable election. Default off keeps single-instance deployments working unchanged. |
| `DIRQ_DB_URL` env / `db_url:` config | `sqlite://...` | Must be a `postgres://` or `postgresql://` URL for HA. |
| `DIRQ_POD_ID` env / `pod_id:` config | hostname | Pod identifier recorded in the `server_peers` table. In Kubernetes, set from the downward API to `metadata.name`. |

### Endpoints

| Endpoint | Auth | Meaning | Returns 200 when |
|---|---|---|---|
| `/healthz` | none | Liveness — the process is alive and responsive. | Always (HTTP server is up). |
| `/readyz` | none | Readiness — this pod is the leader. | Election disabled, **or** this pod currently holds the lock. |

`/healthz` deliberately does **not** depend on leader state. A standby
pod is healthy; it just isn't ready. Coupling the two would cause
Kubernetes to restart standbys unnecessarily.

## Failover behavior

With default tunings:

```
t=0          leader pod dies, network partitions, or Postgres conn breaks
t=0          Postgres releases the advisory lock (side-effect of TCP close)
t=0–2s       a standby's polling tick fires; pg_try_advisory_lock returns true
t=2–4s       standby's next /readyz returns 200; old leader's /readyz returns 503
t=4–6s       endpoint controller updates the Service's Endpoints object
t=4–8s       the OpenShift router observes the change and reroutes new traffic
t=5–30s      agents notice their gRPC stream is dead, reconnect through the
             same external endpoint, and land on the new leader
```

End-to-end RTO is typically **15–30 seconds**. Most of the budget is
agent reconnect backoff, not platform-side cutover.

In-flight gRPC streams to the dead pod do **not** migrate. They break,
the agent reconnects (with bounded exponential backoff), and the new
leader accepts the fresh stream.

## OpenShift deployment

The OpenShift manifests below are illustrative — a Helm chart is the
right long-term packaging. Adapt namespaces, image refs, secret names,
and storage to your environment.

### Required secrets

A shared `Secret` containing the items that **every pod must agree on**:

- TLS server cert + key for the gRPC listener
- The CA bundle used for mTLS client-cert verification
- The signing key used to sign control messages to agents
- The registration secret used by new agents on first connect

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: dirq-server-creds
type: Opaque
stringData:
  server.crt: |
    -----BEGIN CERTIFICATE-----
    ...
  server.key: |
    -----BEGIN PRIVATE KEY-----
    ...
  ca.crt: |
    -----BEGIN CERTIFICATE-----
    ...
  signing.key: |
    ...
  registration_secret: "..."
```

Every pod mounts the same `Secret` at the same paths. Pods MUST share
these — otherwise an agent that reconnects to a different pod will
either reject the server's TLS cert or fail to verify a signed message
from a key it hasn't seen.

### Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dirq-server
spec:
  replicas: 2
  selector:
    matchLabels: { app: dirq-server }
  template:
    metadata:
      labels: { app: dirq-server }
    spec:
      terminationGracePeriodSeconds: 30
      containers:
        - name: dirq-server
          image: quay.io/atgreen/dirq-server:latest
          env:
            - name: DIRQ_POD_ID
              valueFrom: { fieldRef: { fieldPath: metadata.name } }
            - name: DIRQ_LEADER_ELECTION
              value: "true"
            - name: DIRQ_DB_URL
              valueFrom: { secretKeyRef: { name: dirq-pg, key: url } }
            - name: DIRQ_REGISTRATION_SECRET
              valueFrom: { secretKeyRef: { name: dirq-server-creds, key: registration_secret } }
          ports:
            - { name: grpc, containerPort: 50051 }
            - { name: http, containerPort: 8080 }
          livenessProbe:
            httpGet: { path: /healthz, port: http }
            periodSeconds: 10
          readinessProbe:
            httpGet: { path: /readyz, port: http }
            periodSeconds: 2
            failureThreshold: 2
            timeoutSeconds: 2
          volumeMounts:
            - { name: creds, mountPath: /etc/dirq, readOnly: true }
      volumes:
        - name: creds
          secret: { secretName: dirq-server-creds }
```

Two notes on the probe configuration:

- The readiness probe period (`2s`) and `failureThreshold: 2` give
  about 4 seconds for the endpoint controller to react after lock
  release. Drop the period to 1s for sub-second cutover at the cost of
  more probe traffic.
- The liveness probe uses `/healthz`, not `/readyz`, so a healthy
  standby is never restarted just because it isn't leader.

### Pod disruption budget

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: dirq-server
spec:
  minAvailable: 1
  selector:
    matchLabels: { app: dirq-server }
```

Prevents OpenShift from voluntarily evicting both pods at once (during
a node drain, for example).

### Service & Route

```yaml
apiVersion: v1
kind: Service
metadata:
  name: dirq-server
spec:
  selector: { app: dirq-server }
  ports:
    - { name: grpc, port: 50051, targetPort: grpc, appProtocol: grpc }
    - { name: http, port: 8080, targetPort: http }
---
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: dirq-grpc
spec:
  to: { kind: Service, name: dirq-server, weight: 100 }
  port: { targetPort: grpc }
  tls: { termination: passthrough }
---
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: dirq-http
spec:
  to: { kind: Service, name: dirq-server, weight: 100 }
  port: { targetPort: http }
  tls: { termination: reencrypt }
```

The gRPC Route MUST use `passthrough` — OpenShift's HAProxy router
only supports gRPC over a passthrough Route (the router can't re-encrypt
HTTP/2 streams reliably). The HTTP Route can use `edge` or `reencrypt`
as appropriate.

Because of the readiness probe, the Service's `Endpoints` object only
contains the leader's pod IP. The router subscribes to the
Endpoints and updates its backend list when the IP changes — typically
within 1–2 seconds of the readiness probe transition.

### Database

PostgreSQL is required for HA. SQLite has no advisory-lock primitive
suitable for cross-process election; the SQLite backend's leader is a
no-op that always reports leadership, which is correct for a
single-instance deployment but incorrect for multi-pod.

The Postgres instance is the single point of failure that DirQ's HA
*doesn't* address. Use a managed Postgres (Crunchy PGO, AWS RDS, etc.)
or your own HA Postgres setup — anything that gives Postgres its own
failover story. DirQ uses standard transactional semantics; nothing
about it is Postgres-specific beyond the advisory lock.

## Failure modes

### Brief unavailability during cutover

There is a window (typically 2–6 s) between leader death and a standby
being added to the Service's endpoint set. During that window the
external Route has no backend. REST requests return 503; new gRPC
connections fail to establish. Agents already connected to the dead
leader fail their stream and enter their reconnect backoff. This is
expected and not a correctness problem — every operation in flight is
either idempotent (queries) or has its own retry semantics
(`exec_multi` is fanned out at the server; the client gets back a
clear error for in-flight requests).

### Split-brain ruled out by Postgres

The advisory lock is held by exactly one Postgres session at any time.
If a former leader is network-partitioned from Postgres but still
reachable by agents, its `Ping()` fails on the next poll and it
demotes itself. It cannot continue to serve writes thinking it's
leader because:

- Its `/readyz` flips to 503 within one poll interval, so the Service
  removes it from endpoints.
- All write paths in the server are DB-mediated; without a healthy
  Postgres connection they fail anyway.

Two pods that both think they're leader is not possible at the
Postgres lock layer. The worst case is a former leader that briefly
serves stale reads from its in-memory state before it notices the
demotion — at most one poll interval.

### Postgres failure

If Postgres itself dies, the leader loses the lock (its connection
breaks) and demotes. No standby can promote (no DB). All pods report
`/readyz` → 503. The Service has no endpoints; the Route returns 503;
agents fail to connect. This is correct degraded behavior — DirQ
doesn't pretend to function without its database.

### Network partition between leader and Postgres

Same as the previous case. The leader notices on its next poll (≤2 s),
demotes, drops the dedicated lock connection. Other pods become eligible
to promote as soon as their own Postgres connectivity is healthy.

### Time skew

The advisory lock has no time dependency. Clock drift between pods
does not affect election. This is one of the main reasons we picked
Postgres locks over a TTL-based mechanism (etcd lease, K8s lease).

## Backward compatibility

Leader election is **opt-in**. The default for `DIRQ_LEADER_ELECTION`
is `false`. With the flag unset:

- The leader-election goroutine is not started.
- `s.leader` stays `nil`.
- `/readyz` unconditionally returns 200 (single-instance mode).

Existing single-instance deployments — including all SQLite
deployments — work without any configuration change.

## Operational checklist

Before flipping to HA in production:

- [ ] PostgreSQL backend in use (no SQLite).
- [ ] Server credentials (`server.crt`, `server.key`, CA, signing key,
      registration secret) shipped as a `Secret` mounted identically by
      every pod.
- [ ] `DIRQ_POD_ID` set per pod from the downward API.
- [ ] `DIRQ_LEADER_ELECTION=true` on every pod.
- [ ] `Service` selector covers all pods (no per-pod label needed).
- [ ] `readinessProbe` uses `/readyz`, period ≤ 2 s.
- [ ] `livenessProbe` uses `/healthz`, period 10 s+.
- [ ] PodDisruptionBudget with `minAvailable: 1`.
- [ ] Postgres has its own HA / failover plan.
- [ ] Tested: kill the leader, watch `oc get endpoints dirq-server -w`
      shift to the standby within 6 s and agents reconnect.
