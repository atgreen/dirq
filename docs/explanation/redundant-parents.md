# Redundant parents & recovery

When an agent's relay parent goes down, its whole subtree would be cut off from the server. DirQ avoids that by handing every agent pre-chosen fallback parents and a fast, ordered recovery path.

Each non-zone-leader agent receives 2 fallback parent addresses during
registration, chosen from different branches of the tree — and, where
possible, from a different failure domain and off recently-flapping nodes
(see [Reboot-Aware Placement](reboot-aware-placement.md)). On parent failure:

1. Try fallback parent 0 (different branch, sub-second)
2. Try fallback parent 1 (another branch)
3. Ask the server for a new parent assignment via `RequestPeers` RPC

Agents never fall back to direct server connections — they always ask the
server where to go. The server marks the dead parent offline and assigns
a healthy replacement. When a zone leader goes offline, the server
immediately reassigns its orphaned children to other healthy nodes.
