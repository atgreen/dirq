# Changelog

All notable changes to DirQ will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [0.22.4] - 2026-05-23

### Fixed

- **Agent dial paths fail closed on TLS credential errors.** `grpcDialOpts`, `registrationDialOpts`, and `peerDialOpts` in `internal/agent/agent.go` previously fell through to `insecure.NewCredentials()` whenever TLS was enabled but credential loading errored (bad CA file, expired cert, parser fault).  A misconfigured agent would silently switch to plaintext on the wire — wrong failure mode for "protect the agents."  All three helpers now return `([]grpc.DialOption, error)` and propagate cred-load failures to the six call sites (register, connectUpstream ZL path, connectUpstream relay path, connectToAddr, renewCert, requestNewParent), each of which surfaces the error rather than dialing insecurely.
- **`RequestPeers` no longer marks healthy relay parents offline.** The pre-fix code used `s.streams[ParentID]` as the parent-liveness check, but that map only holds direct server streams — i.e., zone leaders.  For any relay parent the lookup was unconditionally `false`, and a single leaf calling `RequestPeers` because its relay-parent flapped would mark that relay offline in both `MeshTopology` and the DB, propagating false failure into routing decisions and broadcast accounting.  `RequestPeers` is now scope-limited to finding a new parent; actual parent death continues to be detected by stream-close (zone leaders), `PeerDisconnected` (relays), and the periodic reaper (server-restart / partition cases).
- **Ghost-online agents during zone-leader failover.** When a ZL's `AgentStream` closed, only the ZL was marked offline, but `reassignOrphans` immediately rewrote each direct child's `parent_id` to a healthy ZL.  The reaper then treated those children as reachable via the new ZL's live stream — but they hadn't actually reattached yet.  New broadcasts targeted these ghosts and timed out as did-not-reply.  Two-part fix: (1) `AgentStream` close defer now marks the **entire subtree** offline so topology reflects the genuine unreachability until reattachment proof arrives; (2) `reassignOrphans` no longer commits speculative `parent_id` rewrites — promote-to-ZL still commits (it's committed truth) but reparent-hint now only delivers a `PeerUpdate` to direct-stream children.  Topology is committed when the child actually reattaches via the new `PeerConnected` upstream message (mirror of `PeerDisconnected`), emitted by the receiving relay's `RelayStream` after the child passes Hello verification.  Closes the prior gap where an agent that successfully reattached via a fallback parent stayed permanently offline in server topology — the primary→fallback→RequestPeers cascade only re-registers when *all* attempts fail.

### Changed

- **Removed dead code left over from the v0.22.0 rebalancer purge.** `Server.assignRole` (legacy per-agent role assignment, replaced by the IP-diverse batcher in v0.22.1) and `Server.sendToAgent` (helper for the removed proactive dispatch paths) had no callers and were tripping the unused-symbol lint on CI.  Both deleted; `assignment` struct remains in use by `registration_batcher.go`.

## [0.22.3] - 2026-05-22

### Changed

- **Fact-cache stage marshals outside the global mutex.** The hot path through `handleQueryResult` held `factStageMu` while running `json.Marshal` on each module's response payload — for the `packages` module that's ~50KB and ~5ms per response.  Serialized across the AgentStream receive goroutines this was the dominant scaling cliff: 50k agents × 5ms ≈ 250s of pure marshal time *if it stayed under the lock*.  Marshal now happens before taking the lock; the lock only covers committing prepared entries to the stage map (microseconds).  Multiple receive goroutines can now marshal concurrently instead of queuing.

## [0.22.2] - 2026-05-22

### Fixed

- **Dispatcher race that under-reported received counts on burst broadcasts.** The query/exec/deploy dispatchers exited their loop on `for Remaining() > 0`, but `ClaimAgent` decremented `Remaining` *before* the result was enqueued and consumed.  Under burst arrivals — many `ClaimAgent` calls firing concurrently while the loop was busy encoding — `Remaining()` could race to zero while real results still sat in the channel.  The loop exited and those late items got GC'd, so the CLI saw fewer encoded results than agents accounted for.  Observed in a 25-VH/VM back-to-back test as exec rounds finishing in 0.46–0.69 s with output like `1006/1248 responded; 242 host(s) did not reply` — the hard timeout was nowhere close to firing.  Fix: a non-blocking drain pass at the end of each dispatcher loop after `Remaining()` reaches zero, encoding anything left in the channel before returning.  Hard-timeout and `ctx.Done()` exits skip the drain (abnormal terminations where late items are genuinely unwanted).

## [0.22.1] - 2026-05-22

### Fixed

- **Registration batcher no longer stacks zone leaders on one host via its single-item fast-path.** `flushBatch` had a special case for size-1 batches that routed them through the legacy per-agent `assignRole`, which promotes to ZL whenever `onlineZLs < MaxZoneLeaders` with no IP-diversity check. At realistic registration-jitter rates (~1–2 arrivals per 200 ms batch window), the overwhelming majority of batches are size 1, so the fast path won every race and filled all ZL slots from whichever VM's VHs arrived first. Observed on a 50×50 fleet: 4 of 5 ZLs on one VM; that VM saturated and every broadcast hit the 60 s hard timeout with ~22 % of agents still pending. Fix: route every batch (including size 1) through the diversity-aware path. A size-1 batch from a ZL-free IP still promotes; a size-1 batch from a ZL-holding IP falls through to relay assignment.

## [0.22.0] - 2026-05-22

### Removed

- **The proactive topology rebalancer is gone.** The 30 s ticker that ran `promoteOneRelay` / `demoteOne` / `redistributeOne` was solving problems the rest of the system already handles — new agent registrations naturally fill empty ZL slots via the burst-aware batcher with source-IP diversity, and the orphan-promotion fallback in `RequestPeers` / `reassignOrphans` covers tree saturation.  What the proactive paths *did* keep causing was mid-broadcast agent disruption (the "agent went away during reassignment" failures), repeated undo of the batcher's IP-diversity work over each tick, and ~50 lines of demote-cooldown / reassigning-set plumbing to dampen the rebalancer's own feedback loops.  Net delete of 395 lines.  Reactive recovery is unchanged: `reassignOrphans` still runs from `AgentStream`'s close defer when a node dies — direct children get reassigned (or promoted on saturation) and the rest of the subtree keeps its existing peer streams.

### Why this is safe

Two invariants verified by tracing:

- **A zone leader dies →** server's `AgentStream` close defer fires `reassignOrphans` synchronously.  Direct children get reassigned to other zone leaders via `FindShallowestParentWithRoom`, or promoted to zone leader themselves if the tree is saturated.  Deeper descendants don't lose their streams (their immediate parent — a depth-1 relay — is still alive, just reconnecting upstream).  Slot fills as new agents register, or stays empty if no new agents arrive (a 4-of-5 ZL fleet still handles its load fine).
- **A relay dies →** its parent detects the peer-stream close and sends `PeerDisconnected` upstream.  Server marks the subtree offline and notifies in-flight broadcasts.  The dead relay's children see their own upstream break, run `connectLoop` → fallback parents → `RequestPeers`, and rehome to a healthy parent — promoted to ZL via the orphan-promotion path if the tree is saturated.

### Operational note

`max_zone_leaders` and `max_children` still mean what they meant — they're upper bounds on new registration assignment.  The server no longer actively maintains the ZL count at exactly `max_zone_leaders`; over a long quiet period after a ZL death, the count may sit one below max until the next registration burst.  That's fine; the remaining ZLs serve the existing fleet, and the burst itself will provide a fresh-IP candidate.

## [0.21.2] - 2026-05-22

### Fixed

- **Registration batcher no longer stacks zone leaders on the same source IP.** The v0.21.1 batcher had a second pass that filled remaining ZL slots from same-IP candidates when a batch contained fewer distinct IPs than open slots — which defeated the whole point of source-IP diversity.  Observed in the latest 50×50 emulation: 4 of 5 zone leaders landed on one VM despite the batcher firing.  The fix removes the second pass entirely (leftover slots wait for batches with different IPs, or for the rebalancer's "promote a relay with children" path 30s later) and adds a relay-only commit path for the non-ZL slice of multi-agent batches that bypasses `assignRole`'s "openZL > 0 → promote" step so the same-IP candidates can't be promoted by the fallback either.

## [0.21.1] - 2026-05-22

### Added

- **Burst-aware registration batching with source-IP diversity for zone leaders.** Registration RPCs now flow through a small in-memory queue (default 200ms window, 200 max batch) and role assignment runs over the whole batch at flush time.  Multi-item batches prefer one zone leader per distinct (and ZL-free) source IP, spreading ZLs across distinct hosts in a single decision instead of letting whichever VM won the lock-contention race grab them all.  Lone registrations fast-path through the existing per-agent assignRole — no change to steady-state behavior.  Useful in production any time you have a co-located burst (rack reboot, post-maintenance restart, deployment rollouts); same code path that made 50,000-agent fleet-scale emulation finally converge with ZLs on 5 different VMs.
- **`/api/v1/debug/inflight` per-zone-leader breakdown.** For every in-flight broadcast (query/exec/deploy), the response now groups the still-pending agent set by zone leader and includes: subtree size, received count, pending count, stream-send-buffer depth, and response-arrival counters for the last 1/5/30 seconds.  Surfaces "which ZL is the bottleneck" without grepping logs.
- **`dirq debug inflight` CLI now renders the per-ZL breakdown.** Marks the chokepoint ZL with `← bottleneck (send_buf full)` when its send buffer is at capacity, and the per-window arrival counts let you distinguish a slow-but-progressing broadcast from one that's actually stuck.

## [0.21.0] - 2026-05-22

This release replaces the broadcast dispatchers' idle-timeout heuristic with explicit per-target accounting tied to mesh-state signals.  Long-running fleet commands (e.g. `dnf install` across thousands of hosts) no longer get cut off at 30 s of silence between fast and slow responders; unreachable agents get retired the moment the server learns they're gone instead of pinning the dispatcher to the hard timeout; **users can now set `--timeout` arbitrarily long without artificial caps**.

### Changed

- **Broadcast dispatchers (query, exec, deploy) no longer use idle timeouts.** Completion is driven by per-target accounting: a session's loop runs until `pending` is empty.  Real responses and synthetic disconnect failures pass through one shared first-terminal-wins gate (`sessionAccounting.ClaimAgent`) so each target is counted at most once.  The hard timeout becomes a true safety net at `command_timeout + 30s transport grace` and rarely fires in practice — stream-loss notifications retire unreachable agents long before it would.
- **Stream loss now drives dispatcher completion via four signal paths:** `AgentStream` close (synthesizes failures for the whole subtree below the lost ZL); `PeerDisconnected` from upstream relays (now also calls `MeshTopology.MarkSubtreeOffline` rather than only touching the DB); the reaper (walks the in-memory topology, notifies dispatchers about any online agent whose ZL ancestor lost its stream); fanout failures at dispatch time (when a ZL's send buffer is full, the subtree it would have relayed to is immediately accounted as failed instead of waiting forever).
- **`/api/v1/debug/inflight` now includes deploy sessions** with target/received/missing counts to match exec and query.
- **`dirq hosts graph --dot` renders left-to-right** (`rankdir=LR`) so large fleet trees fit usefully on screen.

### Fixed

- **No more `1/2490 completed` lies on long-running broadcasts.** The old idle timeout fired at 30 s of silence — for a `dnf install` taking 30+ s per host, the dispatcher exited after the first local response landed, claiming completion while the broadcast was actually still in progress on 2,489 other hosts.
- **AWS test fleet stops losing ~0.4 % of VHs to ephemeral-port collisions.** Linux userdata now reserves ports 50052-51051 via `net.ipv4.ip_local_reserved_ports` before `dnf install` runs.  Sized for up to 1,000 VHs per VM (50,000-agent fleets).

### Notes

The design was reviewed against codex's "first terminal event wins per agent" critique.  Notable invariants the implementation preserves:

- One synchronized gate (`ClaimAgent`) covers both real responses and synthetic disconnect injections so the dispatcher never counts an agent twice.
- Lock hygiene: snapshot session pointers under each global session mutex, release, then call per-session `markGone` inline.  Result channels are never written while holding the global session lock — so stream close handlers don't get blocked by a slow dispatcher.
- Subtree walks in `MeshTopology` are iterative (not recursive) and snapshot under a single RLock pass.
- Synthesized failures count toward completion but are omitted from returned results — surfaced via the existing `missing` field added in 0.20.3.

## [0.20.3] - 2026-05-22

### Fixed

- **Broadcast queries no longer claim "completed" when most of the fleet didn't respond.** The query dispatcher had a 1-second idle timeout — when no responses arrived for 1s it returned whatever it had with `Status: "completed"`. At fleet scale that's a lie: a 2,500-host broadcast routinely returned 700–900 responses in the first second, the dispatcher gave up, and the CLI confidently printed `911/911 completed` because `dirq exec`'s field-resolution phase (which uses the same dispatcher) saw only the agents that beat the idle window. Three structural changes: `dispatchQuery` now returns a `dispatchOutcome` with explicit `Complete`/`IdleTimedOut`/`HardTimedOut` flags and `Responded`/`TotalTargets` counts; the HTTP handler sets `Status: "incomplete"` and surfaces a new `missing` field whenever the wait ended early; the idle window scales with the target count (2s floor + 1s per 500 targets, capped at 30s) so a wider fan-out gets the time it needs without making small fleets wait.
- **`dirq exec WHERE <field-condition>` reports honest coverage.** The exec broadcast header carries a new `unresolved_targets` count from the field-resolution query, and the CLI prints `Plus N host(s) excluded from the broadcast because field-resolution didn't get a response — actual coverage is partial.` when it's non-zero. Stops the previous `911/911 completed` lie when the underlying field-resolution dispatcher gave up early. The final tail line is also honest now: `716/2493 responded; 1777 host(s) did not reply (mesh timeout or unreachable)`.
- **`dirq select` and `dirq query` print the missing count.** Status line reads `Status: incomplete | Targets: N | Received: M | Missing: K (mesh timeout or unreachable)` when the dispatcher couldn't account for everyone.

## [0.20.2] - 2026-05-22

### Fixed

- **Agent registration no longer takes O(N²) under fleet load.** Every Register call used to take a global Go mutex while running two recursive-CTE walks of the whole `agents` table (`FindShallowestParentWithRoom` + `FindFallbackParents`). With a few thousand concurrent registrations the per-call cost grew to hundreds of milliseconds and the queue stretched to **10+ minutes**, leaving most agents stuck in a pre-assignment limbo (`role='leaf'`, `parent_id=NULL`) where they couldn't be routed to and broadcast queries silently counted them as "non-RHEL". The live mesh shape now lives in a new in-memory `MeshTopology` (RWMutex-protected maps for nodes, zone leaders, parent/child links, depth cache) and every assignment is sub-microsecond. The Register hot path no longer touches `WithTopologyLock` at all. Migrated every other topology call site too — RequestPeers, stream open/close lifecycle, rebalancer (`promoteOneRelay`, `demoteOne`, `redistributeOne`, `sendToAgent`, `reassignOrphans`), and `dirq debug stream`. `/api/v1/status` now computes depth and orphan counts in O(N) against the in-memory depth cache instead of the previous O(N²) nested scan.

### Changed

- **`agents.role` and `agents.parent_id` are now best-effort snapshots, not authoritative state.** A new background goroutine writes them back every 30 seconds for operator visibility and cross-restart UX; nothing reads them for routing. On startup, the server rehydrates `MeshTopology` from the DB so `dirq hosts list / graph` aren't blank during the few seconds between restart and agent reconnect. `/api/v1/hosts` overlays the in-memory topology onto each record before serializing, so the CLI always shows live truth regardless of how fresh the snapshot is.

## [0.20.1] - 2026-05-22

### Fixed

- **Orphaned agents (parent_id NULL but role=leaf/relay) are now promoted to zone leader instead of being silently stranded.** When the mesh tree saturates under churn — a zone leader's stream flapping, a wave of children re-registering, a parent with no remaining capacity — `RequestPeers` and `reassignOrphans` used to clear the agent's `parent_id` and move on. The agent kept its non-ZL role but had no route, which meant broadcast queries (`dirq cve`, `dirq exec WHERE …`, etc.) fanned out through zone-leader streams and silently missed those orphans. Both paths now route the orphan into the same escape hatch `assignRole` already had: set role=zone_leader, signal the agent via a new `PeerResponse.new_role` field (in-band on `RequestPeers`) or `PeerUpdate(NewRole=ZONE_LEADER, NewParentAddr="")` (best-effort on the live stream from `reassignOrphans`), and reconnect to the server directly. `assignRole` preserves an existing zone_leader+NULL-parent assignment on re-register so the promotion isn't undone the next time the agent reconnects.
- **`dirq cve` and `dirq errata` no longer label every unreachable host as "non-RHEL".** The count was computed as `online_agents - assessedHosts` and reported as `N not assessed (non-RHEL)`, which conflated three different cases: actually non-RHEL hosts, RHEL hosts that timed out, and RHEL hosts the mesh couldn't reach. The line now splits the count using the agent record's `os` field — output reads `N vulnerable, N patched[, N non-RHEL][, N RHEL did not respond]`, so a non-zero "did not respond" surfaces as the fleet-health signal it actually is, instead of being silently absorbed into a misleading "non-RHEL" total.

### Added

- **`DIRQ_REGISTRATION_JITTER_SECONDS` agent setting** (config key: `registration_jitter_seconds`) caps a random startup delay applied before the first `Register` call. Smooths thundering-herd boot scenarios: multi-VH emulation auto-picks a sensible default (N/4 seconds, clamped to 5–60s), production fleets can opt in for rack-reboot or post-maintenance fleet restarts. Zero disables jitter, preserving previous behavior. The AWS test-fleet script sets `registration_jitter_seconds: 30` when `DIRQ_REPLICAS_PER_VM > 1`.

## [0.20.0] - 2026-05-22

### Added

- **One `dirq-agent` process can now host N virtual hosts** for fleet-scale emulation. Set `DIRQ_VIRTUAL_HOSTS=N` plus `DIRQ_HOSTNAME_PREFIX=<prefix>` and the agent spawns N in-process goroutines, each presenting itself to the server as an independent host with its own agent_id, session token, mTLS client cert, gRPC connection upstream, and downstream relay listen port (`base+i`). Synthesized hostnames are `<prefix>-NNNNN`. Per-instance mTLS certs live under `$DATA_DIR/tls/instances/<hostname>/` so siblings can't clobber each other. The new `make aws` knob `DIRQ_REPLICAS_PER_VM=N` wires this through to the EC2 fleet — e.g., `DIRQ_REPLICAS_PER_VM=1000 LINUX_COUNT=50 make aws` produces 50,000 emulated hosts on 50 VMs, with the security group's relay-port range auto-widened to cover `50052..50051+N`. The relay listener now binds synchronously in `Run()` before registration so port collisions surface as a `Run()` error instead of vanishing into a background goroutine while the agent looks healthy. Windows VMs stay single-tenant (multi-VH is Linux-only).

### Fixed

- **Bare-hostname `WHERE hostname = 'foo'` queries no longer silently broadcast to the entire fleet.** The server's pre-filter only recognized `tag.*` conditions, so a hostname predicate fell through to "match every agent" and dispatched the query to every connected host instead of just the named one. Hostname conditions are now matched against the agents table during pre-filtering alongside tag conditions, so queries scale correctly with the fleet's hostname cardinality.

## [0.19.0] - 2026-05-21

### Added

- **`dirq debug` diagnostic subcommand tree** for troubleshooting fleet, mesh, and request issues without attaching a debugger. `dirq debug inflight` lists every exec, query, and file-op session the server is currently coordinating, with the still-missing agent set for broadcast operations — so you can answer "what is the server waiting for?" at a glance. `dirq debug path <hostname>` walks an agent's mesh parent chain from the DB and flags broken links. `dirq debug stream <hostname>` reports the server's in-memory view of how it would reach an agent (directly connected vs routed via a zone leader). `dirq debug ping <hostname>` sends a no-op exec through the mesh and reports round-trip timing — the only diagnostic that proves a message actually reaches the agent right now. All three lookup tools form a hierarchy of trust: `path` (fastest, DB-only), `stream` (live process state), `ping` (slowest, but truthful). Endpoints are admin-scoped.

### Changed

- **Fact-cache writes now batch and coalesce instead of single-row upserting per query result.** The previous design queued every successful `QueryResult` onto a 4096-slot channel drained by 8 workers issuing one `INSERT … ON CONFLICT` per agent-module — which silently dropped facts at the channel head under broadcast queries against large fleets, and serialized writers under SQLite's global writer lock. Query results now stage into an in-memory map keyed by `(agent_id, module)` and a single batcher goroutine flushes every 250ms (or when 5000 distinct keys are staged) via a new bulk-upsert path: Postgres uses `unnest()` of typed arrays in one round-trip; SQLite emits chunked multi-row `VALUES` inside a single transaction so the writer lock is taken once per flush. Three new server knobs tune the behavior — `DIRQ_FACT_FLUSH_INTERVAL` (default 250ms), `DIRQ_FACT_FLUSH_SIZE` (5000), `DIRQ_FACT_STAGE_CAP` (20000). The hard cap drops only *new* keys on saturation; existing-key overwrites are always free since coalescing is the same last-write-wins semantic the DB upsert already had. Tested in scope for fleets up to 500k nodes.
- **`dirq run` Python interpreter probe output is now summarized by interpreter path** instead of printing one line per host — so a 50k-host run reports `/usr/bin/python3.9 (50000 host(s))` rather than 50000 individual lines. Detected paths are still persisted as `ansible_python_interpreter` tags on each host so subsequent runs skip the probe entirely. The status message also now reads "non-Windows host(s)" to match what the gate actually does (Windows is excluded; macOS/BSD would be probed too).

## [0.18.0] - 2026-05-20

### Added

- **Postgres advisory-lock leader election for HA deployments** — set `DIRQ_LEADER_ELECTION=true` and run N pods against a shared PostgreSQL database; exactly one pod holds the lock at any time and the others stay warm. The new `GET /readyz` endpoint returns 200 on the leader and 503 on standbys, so the Kubernetes/OpenShift endpoint controller automatically routes traffic only to the current leader. Failover RTO is typically 15–30s, most of which is the agent reconnect window. Backward-compatible: default is off; existing single-instance and SQLite deployments are unchanged.
- **`HA.md`** at the repo root walks through the active/standby model, the lock mechanism, failover timeline, OpenShift manifests (Deployment, PDB, Service, gRPC passthrough + HTTP reencrypt Routes), failure modes (split-brain analysis included), and a pre-prod checklist.

## [0.17.15] - 2026-05-19

### Added

- **`ansible.windows` is now bundled in the dirq EE** — ansible-core only ships `ansible.builtin`, so Windows playbooks using `ansible.windows.win_ping` / `win_shell` / `ansible.windows.setup` failed at parse time in the EE. Adding the collection to `requirements.yml` means the EE is ready for Windows targets out of the box.

## [0.17.14] - 2026-05-19

### Fixed

- **Restored `os_linux` / `os_windows` family groups in `/api/v1/inventory`** — when the agent started reporting the Linux distro as `agent.OS` ("redhat", "fedora", ...) instead of the family in 0.17.0, the server silently switched from emitting `os_linux` / `os_windows` groups to per-distro groups (`os_redhat`, `os_fedora`, ...), breaking `hosts: os_linux` in every playbook that relied on it. The server now exposes a `dirq_os_family` hostvar and emits both an `os_<family>` group (containing all hosts of that family) and the per-distro `os_<distro>` group as a child of the family group — so `hosts: os_linux` works again *and* distro-specific targeting still works.
- **`atgreen.dirq` inventory and cache plugins set wrong `ansible_system` / `ansible_os_family`** — both assumed `dirq_os` was a family name, so since 0.17.0 they were setting `ansible_system: Win32NT` on RHEL/Fedora/etc. hosts. Both now consume the server's `dirq_os_family` hostvar (with a fallback for older servers). The inventory plugin's `_detect_distro` also consults the agent-reported distro string first, which is authoritative when present.

## [0.17.13] - 2026-05-19

### Fixed

- **EE base image was wrong (again) — settled on UBI9 per Red Hat guidance** — `community-ee-minimal:latest` isn't pullable anonymously, and both `awx-ee` and `community-ee-minimal` are finished EE images meant to be *consumed*, not built upon. Per the ansible-builder documentation's list of supported bases, switched to `docker.io/redhat/ubi9:latest` (Red Hat's free Universal Base Image, anonymous pull, explicitly published as the base for derived images). `ansible-builder` now installs `python3.11`, `ansible-core`, and `ansible-runner` itself. AAP customers on a paid subscription can swap in `registry.redhat.io/ansible-automation-platform-25/ee-minimal-rhel9`.

## [0.17.12] - 2026-05-19

### Fixed

- **EE build failed because `awx-ee` was the wrong base for layering** — `awx-ee` is a kitchen-sink image with many pre-installed collections; `ansible-builder` on top of it re-resolves their transitive Python deps (e.g. `ovirt-imageio`), some of which need to compile from source. The build died with `gcc: No such file or directory` because `awx-ee` doesn't ship a compiler. Switched the base to `quay.io/ansible/community-ee-minimal:latest` — the upstream-equivalent of Red Hat's `ee-minimal-rhel9` (which requires an AAP subscription) and the canonical base for layering: Python, `ansible-core`, `ansible-runner`, nothing else to fight with.

## [0.17.11] - 2026-05-19

### Fixed

- **EE build failed at ansible-builder's final-image `check_ansible` step** — the validator runs `import ansible` and `import ansible_runner` against `/usr/bin/python3` and aborts the build if either is missing, even when the base image already has them. `execution-environment.yml` now declares both under `dependencies.ansible_core` / `dependencies.ansible_runner`, satisfying the check (pip install is a no-op on `awx-ee` since they're already present).

## [0.17.10] - 2026-05-19

### Fixed

- **EE image was built on an obsolete base** — the hand-rolled Containerfile from 0.17.8 inherited `quay.io/ansible/ansible-runner:latest`, an old CentOS 8 image with ansible-core 2.12 baked in. Jobs run in the EE failed with `Collection atgreen.dirq does not support Ansible version 2.12.5.post0` and the inventory plugin's groups (e.g. `os_linux`) weren't honored. EE is now built with `ansible-builder` against `quay.io/ansible/awx-ee:latest` (the official AWX/AAP base, shipping modern ansible-core).

### Changed

- **EE build uses ansible-builder** — CI (`.github/workflows/ee.yml`) and local devs share one canonical `execution-environment.yml` at the repo root; no more drift between the file local devs run `ansible-builder` against and what CI ships. Local builds: `make ee`.

## [0.17.9] - 2026-05-19

### Fixed

- **`atgreen.dirq.dirq` cache plugin was non-functional** — three independent contract violations: `fact_caching_connection` was read from a positional arg that Ansible never passes (the URL silently fell back to `DIRQ_SERVER_URL` env or `localhost:8080`); `get()` returned `{}` on miss instead of raising `KeyError`, shadowing real `gather_facts` output with nothing; and `packages`/`services` were nested under `ansible_facts` inside the per-host fact dict, ending up as `ansible_facts.ansible_facts.packages` after Ansible's own wrapping. All three fixed.
- **`atgreen.dirq.dirq` connection plugin ignored `dirq_agent_id`** — the per-host agent ID set by the inventory plugin was looked up via `_variable_manager` (not a real `ConnectionBase` attribute) and `_play_context.vars` (empty in modern Ansible), so the lookup always returned `{}` and every connection fell back to a hostname round-trip. The plugin now declares `dirq_agent_id` as a standard option with `vars:` so Ansible's option machinery resolves it from inventory hostvars automatically. The hostname-fallback also switched from a full `GET /api/v1/hosts` scan to the single-host `GET /api/v1/hosts/{hostname}` endpoint.
- **`atgreen.dirq.dirq` inventory plugin: dead `inventory_cache` doc fragment removed; `verify_file` tightened to require a `dirq.yml`/`dirq.yaml` suffix** (matching the standard Ansible inventory-plugin convention); query-filter result handling guards against missing `hostname` fields. README example renamed accordingly to `inventory.dirq.yml`.

## [0.17.8] - 2026-05-19

### Added

- **Ansible Execution Environment image published to ghcr.io** — new `.github/workflows/ee.yml` triggers on tag push and builds `ghcr.io/atgreen/dirq-ee:<version>` (and `:latest`). The `atgreen.dirq` collection is built from the workspace tree at the tag (not pulled from Galaxy), so the EE bundles a collection byte-identical to the release.

### Fixed

- **`dirq doctor` mislabeled the database backend** — the check always read "PostgreSQL ok connected" even on SQLite deployments. The row is now labeled "Database" and the detail reflects the actual backend ("sqlite connected" or "postgres connected"). `/api/v1/status` gained a `database_kind` field driving this.

## [0.17.7] - 2026-05-19

### Fixed

- **Ansible Windows modules still failed after 0.17.6** — the splitter fix was correct but ran against pre-mangled input. Ansible wraps every command (Windows included) in `/bin/sh -c '…'`, and POSIX-escapes a literal `'` inside that wrapper as `'"'"'`. The agent stripped the outer single quotes but left the `'"'"'` sequences intact, so the splitter produced `""<base64>""` (literal double quotes) and PowerShell still dumped its usage screen. The agent now undoes the `'"'"'` escape after stripping the outer quotes, mimicking what the absent shell would have done.

## [0.17.6] - 2026-05-19

### Fixed

- **Ansible PowerShell modules failed with "Failed to create temporary directory" on Windows** — the agent's PowerShell argument splitter preserved surrounding single/double quotes inside each parsed argument, so PowerShell received `-EncodedCommand '<base64>'` (literally, with quotes) and could not decode the payload, dumping its usage screen instead. Every `ansible.windows.*` module was affected. Quotes are now stripped so PowerShell receives the clean token.

## [0.17.5] - 2026-05-19

### Fixed

- **Single-host `exec`/`put_file`/`fetch_file` could hang on agents reachable through the mesh** — `dispatchExec` routed via a single zone leader resolved from the DB's `parent_id` chain; a topology shift between connect events could leave that chain stale, so the message went down the wrong subtree and the target never saw it. Single-host dispatch now falls back to fanning out to all directly-connected agents (same pattern as broadcast exec), and the targeted agent — matched on `agent_id` in the message — is the only one that executes. Direct-connect fast path unchanged.

## [0.17.4] - 2026-05-19

### Fixed

- **Python interpreter cache survives agent reconnect** — `RegisterAgent` was replacing the entire tags map with what the agent reported on every reconnect, wiping server-set tags (notably the `ansible_python_interpreter` cache from 0.17.2). Tags are now merged instead of replaced; the probe truly runs only once per host.

### Changed

- **Tag semantics on agent re-registration** — agent-reported keys still win on conflict, but tags set server-side (via `PATCH /api/v1/hosts/{id}/tags`) now persist across agent reconnects. Removing a tag from agent config no longer drops it from the server; use `DELETE /api/v1/hosts/{id}/tags/{key}` to clear explicitly.

## [0.17.3] - 2026-05-19

### Fixed

- **HTTP 429 from `/api/v1/exec` under Ansible at scale** — single-host exec was sharing the broadcast rate-limit bucket (10/s, burst 20) with `query` and `exec_multi`, so a `--forks 10` run against ~25 hosts would fail most tasks with "Too Many Requests". `/api/v1/exec` now has its own bucket (100/s, burst 500) sized for point-to-point RPC traffic.
- **Ansible plugins retry HTTP 429 with exponential backoff + jitter** — both the standalone connection plugin and the collection's shared `DirQClient` survive transient rate-limit bursts instead of failing the playbook task.

## [0.17.2] - 2026-05-19

### Fixed

- **Standalone Ansible connection plugin SyntaxError** — `_broadcast_content` was left as an orphaned function header (no body) by the cache removal in 0.17.1, causing every Ansible task to fail at import with "expected an indented block after function definition"

### Changed

- **`dirq run` is faster on repeat invocations** — discovered Python interpreters are now persisted as the `ansible_python_interpreter` tag, so subsequent runs skip the probe entirely
- **Query coordination timeout shortened** — server idle timeout in `dispatchQuery` reduced from 5s to 1s; trims the wait for stragglers on every query while keeping a safety net for genuinely unreachable agents

## [0.17.1] - 2026-05-19

### Fixed

- **`WHERE hostname = 'foo'` now filters exec targets** — bare `hostname` is resolved server-side from the agent DB for exec targeting, not just queries
- **Dotted unquoted values in WHERE** — `WHERE hostname = ip-10-0-1-5.ec2.internal` no longer fails with a parse error
- **Windows agent reported wrong OS** — gopsutil Platform on Windows returned a long string instead of "windows", breaking OS-based filtering and the Python probe
- **Python probe required 3.7+** — now requires 3.8+; error message tells RHEL 8 users to install `python39`

## [0.17.0] - 2026-05-18

### Added

- **`dirq grep`** — search log files across the fleet in parallel; uses `grep` on Linux and `Select-String` on Windows; supports `-i` (case-insensitive), `--tail N` (last N lines only), `--become` (sudo); results formatted as HOST / LINE / MATCH table
- **`dirq hosts list` shows distro name** — OS column now shows the distribution (e.g. "fedora", "rhel", "ubuntu") instead of "linux"; agents report distro at registration via gopsutil

### Changed

- **`dirq exec` syntax reworked** — remote command now goes after `--` separator: `dirq exec WHERE tag.env = 'prod' -- ls -la`; eliminates quoting issues and flag conflicts with the remote command
- **`dirq tls` renamed to `dirq cert`** — subcommands: `generate` (with new `--ca`/`--ca-key` for bring-your-own CA) and `rotate`
- **Package service lifecycle** — RPM, DEB, NSIS, and MSI packages no longer start the agent on fresh install (config must be written first); upgrades restart the service if it was already running

### Fixed

- **`WHERE hostname = 'foo'` now works** — bare `hostname` is injected as a top-level key in query data so it can be used in WHERE without the `os_info.` prefix
- **Ansible connection plugin O(N) hostname lookup** — now calls `GET /api/v1/hosts/{hostname}` (single server-side lookup) instead of fetching the entire host list

## [0.16.0] - 2026-05-18

### Added

- **Certificate rotation** — `dirq cert rotate agent_cert` triggers fleet-wide mTLS cert renewal through the mesh; `signing_key` and `ca` rotation types also supported; `--stagger` flag spreads renewals over time to avoid thundering herd on large fleets
- **Automatic cert renewal** — agents renew their mTLS certificate via `RenewCert` RPC when within 30 days of expiry, without re-registering or resetting topology; checked every 12 hours
- **CA rotation** — configure `tls_ca_old` to trust both old and new CAs during a transition window; agents receive the full CA bundle on renewal
- **Signing key rotation** — configure `signing_key_old` / `signing_pub_old` to trust both old and new Ed25519 keys during transition; agents accept signatures from either key
- **Server TLS hot reload** — server reloads its TLS certificate from disk every 60 seconds; send `SIGHUP` for immediate reload; no restart required
- **`dirq cert generate --ca / --ca-key`** — generate server and agent certs signed by your own CA instead of a self-signed one
- **SECURITY.md** — comprehensive security model documentation covering TLS, mTLS, authentication, authorization, message signing, replay protection, exec security, rate limiting, LLM hardening, and rotation procedures

### Changed

- **`dirq tls` renamed to `dirq cert`** — subcommands: `generate` and `rotate`

## [0.15.1] - 2026-05-18

### Changed

- **`dirq hosts list` shows LAST SEEN in local timezone** — timestamps are now converted from UTC to the user's local timezone
- **`dirq hosts list` shows OS version** — agents now report their OS version at registration (requires agent restart to populate)
- **`dirq hosts list` no longer shows ROLE column** — use `dirq hosts graph` for topology
- **`dirq graph` moved to `dirq hosts graph`** — topology is now a hosts subcommand; `[ZL]` indicator removed (layout makes it obvious)
- **Host endpoints accept hostname or ID** — `dirq hosts show fedora`, `dirq hosts facts fedora`, and tag operations now resolve by hostname, not just agent ID

## [0.15.0] - 2026-05-18

### Added

- **Agentic `dirq ask`** — rewritten as an autonomous tool-use loop; the LLM calls fleet management tools (`dirq_query`, `dirq_hosts_list`, `dirq_hosts_facts`, `dirq_cve_scan`, `dirq_graph`) iteratively until it has enough data to answer; tool calls are shown on stdout; supports both Anthropic native and OpenAI-compatible APIs
- **Bare aggregate queries** — `SELECT COUNT(hostname)` without `GROUP BY` now returns a single row with the aggregate result instead of dumping all host data
- **Prompt injection hardening for `dirq ask`** — LLM is restricted to fleet-related questions only; all tool output is treated as untrusted data, ignoring instructions embedded in hostnames, tags, or query results
- **`dirq doctor` config validation** — checks `client.conf` for unknown keys to catch typos

### Changed

- **`dirq ask` suggests but does not execute mutations** — only read-only tools are exposed; the LLM suggests correct `dirq exec` commands with OS-aware examples when changes are needed

## [0.14.1] - 2026-05-18

### Fixed

- **Server panic when agent registers without client cert** — the mTLS interceptor accessed an empty `VerifiedChains` slice when an agent connected without a client certificate (e.g., during initial registration), crashing the server

## [0.14.0] - 2026-05-17

### Added

- **mTLS for gRPC** — server issues a unique TLS client certificate per agent during registration (CN = agent ID); all subsequent gRPC connections require a valid client cert signed by the server's CA; the registration secret becomes a one-time bootstrap token rather than a persistent credential; activates automatically when the CA key is available (auto-generated or via `DIRQ_TLS_CA_KEY`)
- **Agent identity binding** — server, zone leaders, and relay agents verify that the TLS certificate CN matches the claimed agent ID, preventing agent impersonation even with a stolen session token
- **`dirq hosts list` output capped at 10** — large fleets now show the first 10 hosts with a count summary instead of flooding the terminal; use `--all` to see every host

### Changed

- **Generated `agent.conf` no longer includes shared agent cert/key** — each agent receives its own mTLS cert during registration; only the CA cert is embedded for server verification

## [0.13.0] - 2026-05-17

### Added

- **LLM-powered change review** — `dirq exec`, `dirq run`, and `dirq deploy` can now send proposed actions to an LLM for risk analysis before execution; identifies destructive operations, typos, privilege misuse, and scope concerns; supports Anthropic's native API and any OpenAI-compatible endpoint; configure with `DIRQ_LLM_URL`, `DIRQ_LLM_API_KEY`, and `DIRQ_LLM_MODEL`; disabled by default
- **Playbook content gathering** — for `dirq run`, recursively resolves all referenced task files, roles, and handlers and includes their contents in the review prompt

## [0.12.2] - 2026-05-17

### Added

- **Server signing key pinned in agent.conf** — the server now writes its Ed25519 signing public key into the generated `agent.conf`; agents validate the server's key during registration, preventing MITM attacks at enrollment time

### Fixed

- **Non-PowerShell Ansible commands failed on Windows** — the `/bin/sh -c` wrapper stripping worked for PowerShell commands but the stripped result was not used for the `cmd /c` fallback path, causing `type`, `mkdir`, and other commands to fail

## [0.12.1] - 2026-05-17

### Added

- **Auto-detect Python interpreter** — `dirq run` probes Linux targets for a working Python 3.7+ before invoking Ansible; errors clearly if no compatible Python is found
- **Auto-configure Windows shell type** — `dirq run` automatically sets `ansible_shell_type=powershell` for Windows hosts in the generated inventory, no manual tagging required

### Fixed

- **Ansible commands failed on Windows agents** — the agent now strips the `/bin/sh -c '...'` wrapper that Ansible adds to all commands, which has no meaning on Windows
- **Python 3.6 (platform-python) caused Ansible failures** — the interpreter probe now validates Python >= 3.7 and skips `/usr/libexec/platform-python` which is too old for modern Ansible

## [0.12.0] - 2026-05-16

### Added

- **`dirq mcp`** — built-in [Model Context Protocol](https://modelcontextprotocol.io/) stdio server, allowing LLMs like Claude to manage the fleet directly as a tool; exposes 10 tools: host inventory, system facts, tagging, fleet queries, remote execution, CVE scanning, errata checks, KB verification, and topology graph
- **Auto-detect Python interpreter** — `dirq run` now probes Linux targets for a working Python before invoking Ansible, checking `/usr/bin/python3`, `/usr/libexec/platform-python`, and versioned `python3.x` paths; errors clearly if no Python is found

## [0.11.4] - 2026-05-16

### Fixed

- **Windows MSI installed to wrong directory** — MSI was built without `-arch x64`, causing the agent to install to `Program Files (x86)` instead of `Program Files`; the AWS provisioning script then failed to find the executable and exited before writing the server-generated config
- **AWS fleet agents couldn't reach server** — the generated `agent.conf` used the server's internal hostname which Windows instances couldn't resolve; the provisioning script now rewrites it to the server's private IP
- **Ansible PowerShell modules failed on Windows agents** — the agent double-wrapped PowerShell commands in another `powershell -Command` layer, breaking Ansible's `-EncodedCommand` execution

## [0.11.3] - 2026-05-16

### Fixed

- **Windows MSI install failed with error 1603** — the WiX `util:ServiceConfig` custom action tried to set service recovery options before the service was created; removed from MSI, recovery is now configured post-install

## [0.11.2] - 2026-05-16

### Added

- **SPDX SBOM** — release workflow now generates an SPDX JSON software bill of materials and attaches it to each GitHub release

### Fixed

- **Windows MSI install failed with error 1603** — the MSI tried to start the agent service during install, which failed if no config file existed yet; service is now registered but not started, letting the config be written first

## [0.11.1] - 2026-05-16

### Fixed

- **Generated agent.conf had garbled server address** — when `grpc_addr` was `0.0.0.0:50051`, the generated config got `hostname0.0.0.0:50051` instead of `hostname:50051`; same for `client.conf` with `http_addr`

## [0.11.0] - 2026-05-16

### Added

- **`dirq errata`** — check the fleet against Red Hat advisories (RHSA/RHBA/RHEA); fetches advisory data, extracts all CVEs and fixed packages, and reports which RHEL hosts are patched or vulnerable
- **`dirq kb`** — check Windows hosts for installed hotfixes; reports which hosts have or are missing specific KBs
- **`hotfixes` module** — collects installed Windows hotfixes via `Get-HotFix` (kb_id, description, installed_on); supports filtered collection for targeted KB queries

### Fixed

- **API token shown in CLI help** — the `--token` flag displayed the token from `client.conf` as its default value; now hidden from help output
- **CVE not-assessed count was always zero** — the count compared against query results (which only included RHEL hosts) instead of the total online fleet

## [0.10.0] - 2026-05-16

### Added

- **`os_info.distro`, `distro_version`, `distro_family`** — new fields from `/etc/os-release` (e.g., `distro=rhel`, `distro_family=rhel`, `distro_version=8.10`); enables clean filtering by distribution
- **Filtered package collection** — when a WHERE clause specifies package names (e.g., `packages.name = 'kernel'`), agents run `rpm -q kernel` instead of `rpm -qa`, dramatically reducing collection time and mesh traffic
- **No-match responses** — agents that don't match a WHERE clause now send a lightweight "no match" response instead of staying silent; the server counts completions and finishes as soon as all targets have answered, eliminating idle timeout waits
- **`dirq cve --verbose`** — timestamped step-by-step output showing CVE fetch time, query string, and fleet query duration for diagnosing slow scans
- **CVE scan summary includes not-assessed count** — shows how many hosts were skipped (non-RHEL)

### Fixed

- **CVE scanner scanned non-RHEL hosts** — Fedora, Ubuntu, and Windows hosts were compared against RHEL fix versions, producing nonsensical results; now filters to `distro_family = 'rhel'` only
- **CVE scanner compared wrong RHEL versions** — RHEL 8 hosts were compared against RHEL 10 fixes; now matches fix versions to the host's specific RHEL major version
- **CVE scanner reported every installed kernel** — hosts with multiple kernels installed showed one line per kernel; now compares only the running kernel (`os_info.kernel_version`) and shows one line per host
- **CVE scanner included kpatch as a fix** — kpatch-patch is a live-patching workaround, not the actual fix; now filtered out entirely

## [0.9.2] - 2026-05-16

### Added

- **Double quotes and unquoted strings in query DSL** — `WHERE hostname = "fedora"` and `WHERE hostname = fedora` now work alongside the existing single-quote syntax; the shell often strips single quotes, so this avoids a common frustration
- **`ansible_*` tags passed through as Ansible host vars** — tags like `ansible_python_interpreter=/usr/bin/python3.12` set via `dirq hosts tag` are now used in the generated inventory, overriding defaults

### Fixed

- **`dirq run --module` treated WHERE as a playbook** — `dirq run --module ping WHERE ...` tried to run `WHERE` as a playbook file; now correctly treats all positional args as the WHERE clause when `--module` or `--command` is set
- **`dirq run` didn't forward server URL and token to Ansible** — when configured via `client.conf` instead of env vars, the Ansible subprocess didn't receive `DIRQ_SERVER_URL` or `DIRQ_TOKEN`

## [0.9.1] - 2026-05-16

### Fixed

- **Auto-generated server cert only had localhost SANs** — agents connecting by the server's real IP (e.g., 192.168.1.10) got TLS verification failures; cert now includes all non-loopback interface IPs and the hostname

## [0.9.0] - 2026-05-15

### Added

- **Server-generated agent config** — server writes `/var/lib/dirq/agent.conf` on startup with server address, registration secret, and base64-encoded TLS certs inline; copy one file to onboard an agent
- **Server-generated client config** — server writes `/var/lib/dirq/client.conf` with server URL and bootstrap token; copy to `~/.config/dirq/client.conf` or `/etc/dirq/client.conf` for zero-config CLI
- **CLI config file support** — `dirq` reads `server_url`, `token`, and `tls_insecure` from `~/.config/dirq/client.conf` (user-local, checked first) or `/etc/dirq/client.conf`; on Windows: `%APPDATA%\dirq\` then `C:\ProgramData\dirq\`
- **Inline TLS certs in config** — `tls_ca_data`, `tls_cert_data`, `tls_key_data` keys accept base64-encoded PEM; agent materializes them to disk on startup

### Fixed

- **`exec` ignored field-based WHERE conditions** — `dirq exec "ls" WHERE os_info.os = 'linux'` sent to all agents; now runs a query first to resolve matching agents before dispatching
- **Arg flattener broke exec commands with dashes** — `dirq exec "ls -l"` was split into `ls` and `-l` (cobra flag error); flattener now only splits args starting with `SELECT`

## [0.8.0] - 2026-05-15

### Added

- **Rate limiting on query and exec endpoints** — per-token token-bucket limiter (10 req/s, burst 20) prevents a single client from flooding the fleet with broadcast queries
- **Real-time exec progress** — `dirq exec` now shows "X/Y hosts responded..." while waiting for results; server emits NDJSON progress heartbeats every 5 seconds during streaming exec

### Fixed

- **Rebalancer DB-before-send in promotions** — `promoteOneRelay` now updates the DB only after successfully delivering the PeerUpdate message, matching the pattern already used for demotions and redistributions
- **Registration defaults to zone_leader on failure** — topology assignment errors now reject the registration instead of silently creating excess zone leaders; the agent retries with backoff
- **Windows exec race conditions** — scheduled task name and output file are now unique per request (UnixNano suffix), preventing collisions on concurrent `become=true` requests
- **Windows PowerShell injection** — switched the become-user execution path to `-EncodedCommand` (UTF-16LE base64), eliminating metacharacter injection via `$`, backtick, and `$()` in command strings
- **Insecure temp files on Windows** — output file now uses a per-request unique path instead of a predictable hardcoded name, preventing symlink privilege escalation
- **Path traversal in agent file transfers** — `filepath.Clean` applied to all paths in `put_file`, `fetch_file`, and `deploy` before use
- **Fact cache storm** — replaced unbounded goroutine-per-result with a bounded 8-worker pool, preventing DB connection exhaustion on large broadcast queries
- **Inventory N+1 queries** — replaced per-agent `GetFacts` calls with a single bulk `GetAllFacts` query, reducing DB round-trips from N+1 to 2

## [0.7.1] - 2026-05-15

### Added

- **Default server config file** — RPM and DEB packages now install `/etc/dirq/server.conf` with all options documented and commented; marked as `config(noreplace)` so upgrades preserve edits

### Changed

- **RPMs built on AlmaLinux 8** — server binary now links against glibc 2.28, making it installable on both RHEL 8 and RHEL 9

## [0.7.0] - 2026-05-15

### Added

- **`dirq graph`** — display the agent topology tree in the terminal; zone leaders marked `[ZL]`, online/offline status shown with filled/hollow dots
- **`dirq graph --dot`** — emit topology in Graphviz DOT format for rendering with `dot -Tpng`
- **`dirq --version`** — CLI now reports its version (injected at build time via `-ldflags`)
- **`RequestPeers` RPC** — agents that lose their parent ask the server for a new assignment instead of falling back to a direct server connection
- **Orphan reassignment** — when a zone leader goes offline, the server immediately reassigns its children to healthy parents

### Fixed

- **Agents couldn't connect to peer relay servers** — TLS verification failed because agent certs only have `localhost` as a SAN; peer connections now override `ServerName` to match
- **`connectUpstream` silently fell back to server** — relay agents that couldn't reach their parent opened a direct `AgentStream`, hiding the failure; now returns an error so fallback and `RequestPeers` paths are tried first
- **Rebalancer thrashing** — agents demoted to relay that bounced back to a direct connection were re-demoted every 30 seconds; added exponential backoff dampening (1m to 30m)
- **Server used agent-reported IP for `ListenAddr`** — incorrect in Docker/NAT environments; server now overrides with the peer IP observed on the gRPC connection
- **`RequestPeers` marked healthy parents offline** — if an agent was freshly reassigned to a new parent but hadn't connected yet, a second `RequestPeers` call would mark the new (healthy) parent offline; now checks for an active server stream first
- **Graph showed stale parent relationships** — agents connected directly to the server still appeared under their old (dead) parent in the topology

## [0.6.0] - 2026-05-15

### Added

- **SQLite backend** — embedded SQLite database as the default, eliminating the PostgreSQL dependency for single-server deployments; set `DIRQ_DB_URL=postgres://...` to use PostgreSQL instead
- **Field projection and tabular output** — `SELECT os_info.os, packages.name` now returns flat rows with only the requested fields; array modules (packages, services, disk, network) are expanded into individual rows; CLI renders results as aligned tables instead of JSON
- **Windows CLI installer** — NSIS installer (`dirq-cli-VERSION-setup.exe`) installs the CLI binary and connection plugin, adds to PATH
- **macOS client packages** — tarballs for amd64 and arm64 with CLI binary, connection plugin, and LICENSE
- **Ansible connection plugin in all packages** — RPM, DEB, and `make install` now include the standalone connection plugin at `/usr/share/dirq/connection_plugins/`; CLI searches standard install paths automatically
- **`make demo`** — local 10-agent demo fleet using `podman kube play` with TLS, auth, and SQLite; prints bootstrap token for copy/paste setup
- **`dirq doctor`** — checks Ansible version (minimum 2.15), `ansible` CLI availability, and verifies the connection plugin file exists

### Changed

- **Default database is SQLite** — `sqlite:///var/lib/dirq/dirq.db` unless `DIRQ_DB_URL` specifies PostgreSQL; server binary now requires CGO
- **Auth disabled skips all validation** — previously validated stale tokens even when `DIRQ_AUTH_DISABLED=true`
- **Demo uses distinctive ports** — 19080 (REST) and 19051 (gRPC) to avoid conflicts

### Fixed

- **`dirq run` failed with self-signed TLS** — CLI now forwards `--tls-insecure` as `DIRQ_TLS_INSECURE=true` to the Ansible subprocess
- **Containerfile installed binaries to `/usr/local/bin`** — moved to `/usr/bin` for consistency with RPM/DEB packages
- **CLI printed usage text on runtime errors** — `SilenceUsage` set globally so command failures don't dump help
- **Server exited immediately if database wasn't ready** — now retries connection for up to 60 seconds

## [0.5.1] - 2026-05-14

### Fixed

- **RPM/DEB packages now build from source** — spec files and debian/rules compile with `go build` inside the build environment instead of copying pre-built binaries; packages are reproducible from source
- **RPM binaries were not executable** — `cp` replaced with `install -m 0755`
- **Packages installed to `/usr/local/bin`** — moved to `/usr/bin` (standard for distribution packages); systemd service files updated to match
- **LICENSE missing from packages** — included via `%license` (RPM) and as `/usr/share/doc/*/copyright` (DEB) in all three packages
- **Ansible connection plugin lost per-host inventory vars** in some AAP execution paths — restored variable manager access for multi-DC routing
- **Ansible fact cache ignored `fact_caching_connection`** from ansible.cfg — now reads the configured URL before falling back to `DIRQ_SERVER_URL`
- **Ansible collection Python client failed on self-signed TLS** — added `DIRQ_TLS_INSECURE` support

### Changed

- **`make` default target** is now `help` — shows all available targets instead of building

## [0.5.0] - 2026-05-14

### Added

- **`dirq exec`** — execute a command or script across the fleet in parallel with streaming NDJSON results; supports `--become`, `--script`, `--container`, `--timeout`
- **Broadcast deploy** — `dirq deploy` now sends the package through the mesh tree once per link instead of once per host; each relay forwards to its children, only targeted agents write and install
- **Broadcast exec** — `dirq exec` broadcasts through the mesh like queries; one message traverses each link regardless of fleet size
- **Session token authentication** — agents receive a signed, time-stamped session token during registration; server and relay peers verify tokens cryptographically before accepting stream connections
- **Registration secret** — optional `DIRQ_REGISTRATION_SECRET` pre-shared key gates who can register agents with the server
- **Config file TLS/signing support** — `tls_ca`, `tls_cert`, `tls_key`, `tls_insecure`, `tls_disabled`, `signing_key`, `signing_pub` all configurable via config file in addition to environment variables
- **WHERE clause for `hosts list/tag/untag`** — operate on multiple hosts by query instead of one-at-a-time by ID

### Changed

- **Deploy is now parallel by default** — broadcast replaces the per-host rolling wave approach; the `--parallel` flag has been removed
- **Auto-generated keys stored in `/var/lib/dirq/`** instead of `/tmp/` for security (0700 permissions; falls back to user-private temp dir)
- **Bootstrap token written to file** (`/var/lib/dirq/bootstrap-token`) instead of server log to prevent credential leakage
- **API tokens accepted only via `Authorization` header** — query string `?token=` parameter removed to prevent log/proxy credential leakage
- **Auto-generated TLS certs use CA verification** instead of forcing `InsecureSkipVerify`; certs are reused if already present so server and agent share the same CA
- **Exec stdout/stderr base64-encoded** on the wire for binary safety across all exec endpoints

### Fixed

- **Concurrent deploys could overwrite each other** — temp filenames now include a unique deploy ID
- **`hosts list WHERE` made N+1 API calls** — now uses query result data directly (single call)
- **CLI created a new TLS transport per request** when `--tls-insecure` was set, preventing connection reuse
- **Ansible connection plugin lost per-host inventory vars** in some AAP execution paths — restored proper variable manager access for multi-DC routing
- **Ansible fact cache ignored `fact_caching_connection`** from ansible.cfg — now reads the configured URL before falling back to `DIRQ_SERVER_URL`
- **Ansible collection Python client failed on self-signed TLS** — added `DIRQ_TLS_INSECURE` support to the shared API client
- **Makefile default target** changed from `build` to `help` — running `make` now shows available targets

## [0.4.0] - 2026-05-14

### Added

- **`dirq cve`** — scan RHEL systems for CVE vulnerabilities using the Red Hat Security Data API; compares installed package versions against fixed versions with RPM version comparison

## [0.3.0] - 2026-05-14

### Added

- **`dirq select`** — query the fleet with natural syntax (`dirq select hostname WHERE tag.env = 'prod'`), replacing `dirq query`
- **`dirq deploy`** — deploy RPM, DEB, or MSI packages through the relay mesh with depth-first rolling waves by default
- **`dirq doctor`** — validate deployment health: connectivity, auth, database, fleet status, agent version skew, tree topology, local tooling
- **`dirq run` with WHERE syntax** — playbook as first arg with optional WHERE clause (`dirq run deploy.yml WHERE tag.env = 'prod'`), replacing `--query` flag
- **Arg flattening** — quoted multi-word args are split by whitespace, so `dirq "select hostname where tag.env = 'prod'"` works naturally
- **Server status endpoint** — `GET /api/v1/status` returns database health, agent counts, version distribution, and topology stats
- **Makefile** — `make build`, `make test`, `make install`, `make cross`, `make proto`, `make collection`
- **DEB packaging** — Debian packages for dirq, dirq-server, and dirq-agent alongside existing RPMs

### Changed

- **`dirq run`** now takes playbook as first positional arg with optional WHERE clause instead of `--query` flag
- **CLI examples** recommend quoting queries to avoid shell interpretation of `>`, `<`, `*`, `(`, `)`
- **`dirq skill`** output now documents all CLI commands

### Removed

- **`dirq query`** — replaced by `dirq select`

### Fixed

- **GROUP BY on nested module fields returned null** — server-side aggregation now flattens nested module data into dotted keys before GROUP BY

## [0.2.1] - 2026-05-13

### Fixed

- **Exec audit log updates never matched** — `UpdateExecLog` queried by auto-generated ID instead of request ID, leaving audit rows permanently incomplete
- **Token scopes not enforced** — readonly tokens could access exec, tag mutation, and token management endpoints; scopes are now checked per-route
- **GROUP BY aggregation errors silently discarded** — partial failures now return HTTP 500 instead of misleading results
- **Token validation scaled poorly** — every API request scanned all tokens with bcrypt; now uses an indexed prefix for O(1) lookup

## [0.2.0] - 2026-05-13

### Added

- **`dirq ask` command** — natural language queries via LLM, translating plain English to DirQ DSL
- **`dirq skill` command** — run reusable query-and-act recipes
- **`dirq run` command** — query the fleet and run Ansible playbooks in one step
- **Config file support** — YAML configuration for server, agent, and CLI
- **`--tls-insecure` flag** — skip TLS verification for dev/test environments
- **Interactive demo suite** — 20-agent fleet with varied personalities for testing
- **Tree rebalancing** — detect imbalanced relay trees and redistribute subtrees
- **Redundant parent fallback** — agents reconnect through alternate parents for mesh resilience
- **In-mesh result aggregation** — snowball aggregation through the relay tree
- **Windows and Linux packaging** — RPM builds and Windows installer
- **GitHub Actions release pipeline** — CI, cross-platform binary builds, and RPM yum repo

### Changed

- **Query DSL rewrite** — hand-rolled parser replaces previous implementation; `FROM` clause removed (breaking change)
- **Stream-based liveness detection** replaces heartbeat polling for connection health
- **Container base images** switched from Alpine to UBI9

### Fixed

- Relay agent heartbeats not reaching the server
- `ENHANCE_YOUR_CALM` keepalive disconnects
- Topology assignment race condition (now uses PostgreSQL advisory lock)
- Rebalancer feedback loop — limited to one action per cycle
- Agent IP resolution and signed message forwarding in mesh relay

## [0.1.0] - 2026-05-13

### Added

- **Query engine** with SQL-like DSL: SELECT, FROM, WHERE, GROUP BY, ORDER BY
- **7 query modules:** cpu, memory, disk, os_info, packages, network, services
- **IN operator** and array-aware filtering (packages, services, disk, network)
- **P2P relay mesh** with automatic topology management (zone leaders, BFS fill)
- **Ansible inventory plugin** (`atgreen.dirq.dirq`) with nested group hierarchy
- **Query-based inventory filtering** — build Ansible inventories from live DirQ queries
- **Ansible connection plugin** (`connection: atgreen.dirq.dirq`) — run playbooks through the mesh
- **Ansible collection** (`atgreen.dirq`) for AAP with inventory + connection plugins
- **Remote execution** (exec_command, put_file, fetch_file) through the relay mesh
- **Exec audit logging** with AAP job attribution (job ID, template, user)
- **TLS by default** — auto-generated self-signed certs, user-supplied certs, mTLS support
- **API authentication** — required by default, bootstrap token on first startup
- **Message signing** (Ed25519) for server-originated control messages
- **Agent reconnection** with exponential backoff on connection loss
- **gRPC keepalive** and server-side reaper for dead connection detection
- **Host tag management** — REST API and CLI for add/remove/merge tags
- **Multi-datacenter support** — per-DC servers with automatic per-host routing
- **Windows agent** — Windows Service support, PowerShell privilege escalation
- **CLI tool** (`dirq`) — query, hosts, tokens, tags, TLS cert generation
- **Containerfile** — multi-stage build for server and agent images
- **podman-compose** — dev environment with server + PostgreSQL
- **Execution Environment definition** for ansible-builder
- **AAP credential type definition**

### Components

- `dirq-server` — Go, gRPC + REST API, PostgreSQL-backed
- `dirq-agent` — Go, single static binary, Linux + Windows
- `dirq` — Go, CLI tool
- `atgreen.dirq` — Python, Ansible collection

[0.22.4]: https://github.com/atgreen/dirq/releases/tag/v0.22.4
[0.22.3]: https://github.com/atgreen/dirq/releases/tag/v0.22.3
[0.22.2]: https://github.com/atgreen/dirq/releases/tag/v0.22.2
[0.22.1]: https://github.com/atgreen/dirq/releases/tag/v0.22.1
[0.22.0]: https://github.com/atgreen/dirq/releases/tag/v0.22.0
[0.21.2]: https://github.com/atgreen/dirq/releases/tag/v0.21.2
[0.21.1]: https://github.com/atgreen/dirq/releases/tag/v0.21.1
[0.21.0]: https://github.com/atgreen/dirq/releases/tag/v0.21.0
[0.20.3]: https://github.com/atgreen/dirq/releases/tag/v0.20.3
[0.20.2]: https://github.com/atgreen/dirq/releases/tag/v0.20.2
[0.20.1]: https://github.com/atgreen/dirq/releases/tag/v0.20.1
[0.20.0]: https://github.com/atgreen/dirq/releases/tag/v0.20.0
[0.19.0]: https://github.com/atgreen/dirq/releases/tag/v0.19.0
[0.18.0]: https://github.com/atgreen/dirq/releases/tag/v0.18.0
[0.17.15]: https://github.com/atgreen/dirq/releases/tag/v0.17.15
[0.17.14]: https://github.com/atgreen/dirq/releases/tag/v0.17.14
[0.17.13]: https://github.com/atgreen/dirq/releases/tag/v0.17.13
[0.17.12]: https://github.com/atgreen/dirq/releases/tag/v0.17.12
[0.17.11]: https://github.com/atgreen/dirq/releases/tag/v0.17.11
[0.17.10]: https://github.com/atgreen/dirq/releases/tag/v0.17.10
[0.17.9]: https://github.com/atgreen/dirq/releases/tag/v0.17.9
[0.17.8]: https://github.com/atgreen/dirq/releases/tag/v0.17.8
[0.17.7]: https://github.com/atgreen/dirq/releases/tag/v0.17.7
[0.17.6]: https://github.com/atgreen/dirq/releases/tag/v0.17.6
[0.17.5]: https://github.com/atgreen/dirq/releases/tag/v0.17.5
[0.17.4]: https://github.com/atgreen/dirq/releases/tag/v0.17.4
[0.17.3]: https://github.com/atgreen/dirq/releases/tag/v0.17.3
[0.17.2]: https://github.com/atgreen/dirq/releases/tag/v0.17.2
[0.17.1]: https://github.com/atgreen/dirq/releases/tag/v0.17.1
[0.17.0]: https://github.com/atgreen/dirq/releases/tag/v0.17.0
[0.16.0]: https://github.com/atgreen/dirq/releases/tag/v0.16.0
[0.15.1]: https://github.com/atgreen/dirq/releases/tag/v0.15.1
[0.15.0]: https://github.com/atgreen/dirq/releases/tag/v0.15.0
[0.14.1]: https://github.com/atgreen/dirq/releases/tag/v0.14.1
[0.14.0]: https://github.com/atgreen/dirq/releases/tag/v0.14.0
[0.13.0]: https://github.com/atgreen/dirq/releases/tag/v0.13.0
[0.12.2]: https://github.com/atgreen/dirq/releases/tag/v0.12.2
[0.12.1]: https://github.com/atgreen/dirq/releases/tag/v0.12.1
[0.12.0]: https://github.com/atgreen/dirq/releases/tag/v0.12.0
[0.11.4]: https://github.com/atgreen/dirq/releases/tag/v0.11.4
[0.11.3]: https://github.com/atgreen/dirq/releases/tag/v0.11.3
[0.11.2]: https://github.com/atgreen/dirq/releases/tag/v0.11.2
[0.11.1]: https://github.com/atgreen/dirq/releases/tag/v0.11.1
[0.11.0]: https://github.com/atgreen/dirq/releases/tag/v0.11.0
[0.10.0]: https://github.com/atgreen/dirq/releases/tag/v0.10.0
[0.9.2]: https://github.com/atgreen/dirq/releases/tag/v0.9.2
[0.9.1]: https://github.com/atgreen/dirq/releases/tag/v0.9.1
[0.9.0]: https://github.com/atgreen/dirq/releases/tag/v0.9.0
[0.8.0]: https://github.com/atgreen/dirq/releases/tag/v0.8.0
[0.7.1]: https://github.com/atgreen/dirq/releases/tag/v0.7.1
[0.7.0]: https://github.com/atgreen/dirq/releases/tag/v0.7.0
[0.6.0]: https://github.com/atgreen/dirq/releases/tag/v0.6.0
[0.5.1]: https://github.com/atgreen/dirq/releases/tag/v0.5.1
[0.5.0]: https://github.com/atgreen/dirq/releases/tag/v0.5.0
[0.4.0]: https://github.com/atgreen/dirq/releases/tag/v0.4.0
[0.3.0]: https://github.com/atgreen/dirq/releases/tag/v0.3.0
[0.2.1]: https://github.com/atgreen/dirq/releases/tag/v0.2.1
[0.2.0]: https://github.com/atgreen/dirq/releases/tag/v0.2.0
[0.1.0]: https://github.com/atgreen/dirq/releases/tag/v0.1.0
