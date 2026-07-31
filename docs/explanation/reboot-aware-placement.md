# Reboot-aware placement

A host that just rebooted is empirically more likely to reboot again — reboots cluster in time. Because a parent's failure orphans its **entire subtree**, the cheapest place for an unreliable host is a leaf and the worst place is a zone leader. The topology tracks this and steers placement accordingly:

- **Flap score.** Each time a node disappears and re-registers, it earns a point on a time-decayed flap score (`exp(-elapsed / DIRQ_FLAP_WINDOW)`, default 1 h half-life). Once the decayed score crosses `DIRQ_FLAP_THRESHOLD` (default 1.5 — a single one-off reboot doesn't trip it; two within the window do) the node is **on probation**.
- **Probation → leaf.** A probationary node is capped at `DIRQ_PROBATION_CHILD_CAP` children (default 0 → it accepts no new children and stays a leaf) and is deprioritized in parent selection. Existing children are never evicted — the cap only prevents a flaky node from *accumulating* a subtree in the first place, so churn stays confined to the leaves.
- **No flaky zone leaders.** The batcher never promotes a node that's currently on probation to zone leader while a stabler candidate exists.
- **Correlated failure domains.** Reboots also cluster in *space* (a rack loses power, a hypervisor bounces). Nodes are bucketed into failure domains by network prefix (`DIRQ_FAILURE_DOMAIN_PREFIX_V4`/`_V6`, default /24 and /64). When at least `DIRQ_DOMAIN_FLAP_MIN_NODES` members of a domain are individually flapping (default 2), the whole domain is treated as *hot*: even a personally-quiet host in it is deprioritized as a parent, and fallback parents are preferred in a **different** failure domain than the primary (a backup in the same rack is useless when the rack is what dropped).

The personal cap is a hard constraint enforced on every placement; the failure-domain signal is a soft steer that never starves the tree (if every candidate is hot, one is still chosen). All of this is in-memory reliability state — it is not persisted, and after a server restart nodes re-earn their scores within a window or two. Parent selection lives in `MeshTopology.FindShallowestParentWithRoom` / `FindFallbackParents`; the ZL-promotion path is in `registration_batcher.go`. Set `DIRQ_FLAP_THRESHOLD=0` to disable reliability-aware placement entirely (reverting to pure BFS fill).

See [Configuration](../reference/configuration.md) for the tunable knobs and
[Metrics & observability](../reference/observability.md) for the signals this exposes.
