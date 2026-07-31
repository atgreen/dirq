# Completion reporting & result aggregation

## Completion reporting

Broadcast operations (query, exec, deploy) track completion **per target**, not by watching for silence. A session ends when every target has either responded or been positively identified as disconnected — so `dirq exec --timeout 3600 -- yum upgrade -y` runs the full hour even if fast and slow responders leave long gaps between replies. A hard timeout of `command_timeout + 30 s` remains as a safety net.

If some targets can't be accounted for, the CLI reports it rather than claiming success: `Status: incomplete | Targets: N | Received: M | Missing: K`.

## Result aggregation

Query results aggregate in-mesh, not at the server. Each relay buffers results
from its children for 2 seconds, then flushes one `AggregatedQueryResult`
upstream. Zone leaders do the same. The server receives ~5 messages (one per
zone leader) instead of 100k individual responses.
