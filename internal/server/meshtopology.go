// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"sort"
	"sync"

	"github.com/atgreen/dirq/internal/db"
)

// MeshTopology is the in-memory authoritative view of the agent mesh tree.
//
// Why it's in-memory and not in the DB: every registration used to hold a
// global lock while running two recursive CTE walks of the whole agents
// table (FindShallowestParentWithRoom + FindFallbackParents), giving each
// Register call O(N) cost.  At fleet scale that's O(N²) total — for 2,500
// agents it produced 10-minute queues under the topology mutex.  The
// in-memory representation gives O(log N) parent lookups via a min-heap
// over depth+child-count and O(1) for everything else.
//
// The DB columns `agents.role` and `agents.parent_id` are now optional
// snapshots written asynchronously for operator visibility.  The mesh
// itself is reconstructed in seconds whenever the server restarts —
// agents reconnect via their existing reconnect/re-register loop and
// repopulate this struct.  Nothing about HA or pod failover depends on
// reading the snapshot back.
type MeshTopology struct {
	mu          sync.RWMutex
	nodes       map[string]*meshNode
	zoneLeaders map[string]struct{}
	cfg         TopologyConfig
}

// meshNode holds the per-agent state the topology cares about.
// Hostname is mirrored from the DB for log/debug clarity; it never
// changes after registration.
type meshNode struct {
	id         string
	hostname   string
	listenAddr string
	role       string // "zone_leader" or "relay" (drop "leaf" — every non-ZL is a relay node)
	parentID   string // "" for zone leaders
	children   map[string]struct{}
	depth      int  // 0 for zone leaders; parent.depth + 1 otherwise
	online     bool
}

// NewMeshTopology returns an empty topology.
func NewMeshTopology(cfg TopologyConfig) *MeshTopology {
	return &MeshTopology{
		nodes:       make(map[string]*meshNode),
		zoneLeaders: make(map[string]struct{}),
		cfg:         cfg,
	}
}

// AddAgent registers an agent's identity/listen address without assigning
// a role yet.  Idempotent — refreshes listen_addr/hostname if already present.
func (t *MeshTopology) AddAgent(id, hostname, listenAddr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n, ok := t.nodes[id]; ok {
		n.hostname = hostname
		n.listenAddr = listenAddr
		n.online = true
		return
	}
	t.nodes[id] = &meshNode{
		id:         id,
		hostname:   hostname,
		listenAddr: listenAddr,
		children:   make(map[string]struct{}),
		online:     true,
	}
}

// AssignZoneLeader marks an agent as a zone leader (no parent, depth 0).
func (t *MeshTopology) AssignZoneLeader(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.nodes[id]
	if !ok {
		return
	}
	t.unlinkFromParentLocked(n)
	n.role = "zone_leader"
	n.parentID = ""
	n.depth = 0
	t.zoneLeaders[id] = struct{}{}
}

// AssignChild attaches an agent under parentID.  Returns false if the
// parent is unknown, offline, or at capacity.
func (t *MeshTopology) AssignChild(id, parentID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.nodes[id]
	if !ok {
		return false
	}
	p, ok := t.nodes[parentID]
	if !ok || !p.online {
		return false
	}
	if len(p.children) >= t.cfg.MaxChildrenPerNode {
		return false
	}
	t.unlinkFromParentLocked(n)
	delete(t.zoneLeaders, id)
	n.role = "relay"
	n.parentID = parentID
	n.depth = p.depth + 1
	p.children[id] = struct{}{}
	return true
}

// Reparent moves an existing agent to a new parent.  Same constraints as
// AssignChild.
func (t *MeshTopology) Reparent(id, newParentID string) bool {
	return t.AssignChild(id, newParentID)
}

// PromoteToZL promotes an existing agent into a zone leader.  Used by the
// orphan-promotion escape hatch when the tree saturates.
func (t *MeshTopology) PromoteToZL(id string) {
	t.AssignZoneLeader(id)
}

// ClearParent makes an agent into an orphan with no parent — does not
// remove it from the topology.  Used when a parent disappears and the
// caller hasn't decided where the orphan goes yet.
func (t *MeshTopology) ClearParent(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.nodes[id]
	if !ok {
		return
	}
	t.unlinkFromParentLocked(n)
	delete(t.zoneLeaders, id)
	n.role = ""
	n.parentID = ""
	n.depth = 0
}

// MarkOffline flips an agent offline.  Its subtree stays linked, but
// parent-search and counts skip it until it comes back.
func (t *MeshTopology) MarkOffline(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n, ok := t.nodes[id]; ok {
		n.online = false
	}
}

// MarkOnline flips an agent online.
func (t *MeshTopology) MarkOnline(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n, ok := t.nodes[id]; ok {
		n.online = true
	}
}

// Remove deletes an agent (e.g., terminated permanently).  Children
// become orphans; the caller should reassign them.
func (t *MeshTopology) Remove(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.nodes[id]
	if !ok {
		return
	}
	t.unlinkFromParentLocked(n)
	for childID := range n.children {
		if c, ok := t.nodes[childID]; ok {
			c.parentID = ""
			c.depth = 0
			c.role = ""
		}
	}
	delete(t.zoneLeaders, id)
	delete(t.nodes, id)
}

// FindShallowestParentWithRoom returns the shallowest online node with
// fewer than MaxChildrenPerNode children, breaking ties by current child
// count (least loaded first).  This is the BFS-fill algorithm the SQL
// recursive CTE used to do — now an in-memory pass that's O(N) at worst
// but trivially fast because everything lives in maps.
//
// Returns ok=false if no candidate exists (tree saturated).
func (t *MeshTopology) FindShallowestParentWithRoom() (id, listenAddr string, ok bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	bestDepth := -1
	bestCount := 0
	bestID := ""
	bestAddr := ""
	for nodeID, n := range t.nodes {
		if !n.online || n.listenAddr == "" || n.role == "" {
			continue
		}
		c := len(n.children)
		if c >= t.cfg.MaxChildrenPerNode {
			continue
		}
		if bestID == "" || n.depth < bestDepth || (n.depth == bestDepth && c < bestCount) {
			bestID = nodeID
			bestAddr = n.listenAddr
			bestDepth = n.depth
			bestCount = c
		}
	}
	if bestID == "" {
		return "", "", false
	}
	return bestID, bestAddr, true
}

// FindFallbackParents picks up to `count` alternate parents on different
// zone-leader branches than primaryID, for fault isolation.  Returns
// (id, listenAddr) pairs.
func (t *MeshTopology) FindFallbackParents(primaryID string, count int) []db.Agent {
	t.mu.RLock()
	defer t.mu.RUnlock()

	primaryZL := t.zoneLeaderOfLocked(primaryID)

	type candidate struct {
		id         string
		listenAddr string
		hostname   string
		depth      int
		children   int
		zl         string
	}
	var cands []candidate
	for nodeID, n := range t.nodes {
		if nodeID == primaryID || !n.online || n.listenAddr == "" || n.role == "" {
			continue
		}
		if len(n.children) >= t.cfg.MaxChildrenPerNode {
			continue
		}
		zl := t.zoneLeaderOfLocked(nodeID)
		cands = append(cands, candidate{
			id:         nodeID,
			listenAddr: n.listenAddr,
			hostname:   n.hostname,
			depth:      n.depth,
			children:   len(n.children),
			zl:         zl,
		})
	}

	// Prefer candidates on a different ZL branch, then shallowest, then least loaded.
	sort.Slice(cands, func(i, j int) bool {
		ai, aj := cands[i], cands[j]
		iDiff := ai.zl != primaryZL
		jDiff := aj.zl != primaryZL
		if iDiff != jDiff {
			return iDiff
		}
		if ai.depth != aj.depth {
			return ai.depth < aj.depth
		}
		return ai.children < aj.children
	})

	out := make([]db.Agent, 0, count)
	for i := 0; i < len(cands) && len(out) < count; i++ {
		out = append(out, db.Agent{
			ID:         cands[i].id,
			Hostname:   cands[i].hostname,
			ListenAddr: cands[i].listenAddr,
		})
	}
	return out
}

// FindZoneLeader walks an agent's parent chain to the zone leader at
// the root.  Returns ("", false) if the agent has no parent chain
// (orphaned or unknown).
func (t *MeshTopology) FindZoneLeader(id string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	zl := t.zoneLeaderOfLocked(id)
	return zl, zl != ""
}

// FindZoneLeaderAgent is like FindZoneLeader but returns enough fields
// to satisfy callers that want a db.Agent shape.
func (t *MeshTopology) FindZoneLeaderAgent(id string) (db.Agent, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	zl := t.zoneLeaderOfLocked(id)
	if zl == "" {
		return db.Agent{}, false
	}
	n := t.nodes[zl]
	return db.Agent{
		ID:         n.id,
		Hostname:   n.hostname,
		ListenAddr: n.listenAddr,
		Role:       n.role,
	}, true
}

// zoneLeaderOfLocked walks up parent pointers until it hits a zone leader.
// Caller holds the lock.  Returns "" on cycle or missing parent.
func (t *MeshTopology) zoneLeaderOfLocked(id string) string {
	seen := map[string]bool{}
	cur := id
	for cur != "" {
		if seen[cur] {
			return "" // cycle
		}
		seen[cur] = true
		n, ok := t.nodes[cur]
		if !ok {
			return ""
		}
		if n.role == "zone_leader" {
			return cur
		}
		cur = n.parentID
	}
	return ""
}

// unlinkFromParentLocked removes n from its current parent's children set.
func (t *MeshTopology) unlinkFromParentLocked(n *meshNode) {
	if n.parentID == "" {
		return
	}
	if p, ok := t.nodes[n.parentID]; ok {
		delete(p.children, n.id)
	}
}

// CountOnlineZoneLeaders returns the number of currently-online zone leaders.
func (t *MeshTopology) CountOnlineZoneLeaders() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	count := 0
	for id := range t.zoneLeaders {
		if n, ok := t.nodes[id]; ok && n.online {
			count++
		}
	}
	return count
}

// CountChildren returns the number of online children of an agent.
func (t *MeshTopology) CountChildren(id string) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n, ok := t.nodes[id]
	if !ok {
		return 0
	}
	count := 0
	for childID := range n.children {
		if c, ok := t.nodes[childID]; ok && c.online {
			count++
		}
	}
	return count
}

// NodeInfo is a snapshot view exposed for visibility / debug endpoints.
type NodeInfo struct {
	ID         string
	Hostname   string
	ListenAddr string
	Role       string
	ParentID   string
	Depth      int
	Online     bool
}

// Get returns a snapshot of one node, or (zero, false) if not present.
func (t *MeshTopology) Get(id string) (NodeInfo, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n, ok := t.nodes[id]
	if !ok {
		return NodeInfo{}, false
	}
	return NodeInfo{
		ID:         n.id,
		Hostname:   n.hostname,
		ListenAddr: n.listenAddr,
		Role:       n.role,
		ParentID:   n.parentID,
		Depth:      n.depth,
		Online:     n.online,
	}, true
}

// allNodeIDs returns a snapshot of every agent_id known to the topology.
func (t *MeshTopology) allNodeIDs() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.nodes))
	for id := range t.nodes {
		out = append(out, id)
	}
	return out
}

// ZoneLeaderIDs returns a snapshot of all zone-leader agent IDs (online or not).
func (t *MeshTopology) ZoneLeaderIDs() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.zoneLeaders))
	for id := range t.zoneLeaders {
		out = append(out, id)
	}
	return out
}

// SubtreeIDs returns every descendant of rootID (including rootID itself),
// regardless of online state.  Iterative BFS so deep trees don't recurse;
// the walk happens under RLock and returns a snapshot the caller can use
// after the lock is released.  Used by the broadcast-dispatcher notifier
// when a ZL or relay stream closes — every agent in the subtree is
// effectively unreachable until reconnect, so in-flight broadcasts can
// account them as terminal failures and stop waiting.
func (t *MeshTopology) SubtreeIDs(rootID string) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if _, ok := t.nodes[rootID]; !ok {
		return nil
	}
	var out []string
	queue := []string{rootID}
	seen := map[string]bool{rootID: true}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		out = append(out, id)
		n, ok := t.nodes[id]
		if !ok {
			continue
		}
		for childID := range n.children {
			if seen[childID] {
				continue
			}
			seen[childID] = true
			queue = append(queue, childID)
		}
	}
	return out
}

// MarkSubtreeOffline marks rootID and every descendant offline and
// returns the IDs that were online before the call.  Used when a
// PeerDisconnected event propagates up from a relay: the lost child and
// its entire subtree are gone from the mesh's POV until they reconnect.
func (t *MeshTopology) MarkSubtreeOffline(rootID string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.nodes[rootID]; !ok {
		return nil
	}
	var affected []string
	queue := []string{rootID}
	seen := map[string]bool{rootID: true}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		n, ok := t.nodes[id]
		if !ok {
			continue
		}
		if n.online {
			n.online = false
			affected = append(affected, id)
		}
		for childID := range n.children {
			if seen[childID] {
				continue
			}
			seen[childID] = true
			queue = append(queue, childID)
		}
	}
	return affected
}

// ChildrenOf returns the online children of parentID.
func (t *MeshTopology) ChildrenOf(parentID string) []db.Agent {
	t.mu.RLock()
	defer t.mu.RUnlock()
	p, ok := t.nodes[parentID]
	if !ok {
		return nil
	}
	out := make([]db.Agent, 0, len(p.children))
	for childID := range p.children {
		c, ok := t.nodes[childID]
		if !ok || !c.online {
			continue
		}
		out = append(out, db.Agent{
			ID: c.id, Hostname: c.hostname, ListenAddr: c.listenAddr, Role: c.role,
		})
	}
	return out
}

// Rehydrate seeds the topology from a snapshot (e.g., the DB after
// server restart).  Roles other than "zone_leader" or "relay" are treated
// as unassigned.  Children sets are reconstructed from parent_id pointers
// so callers don't need to pre-aggregate.
func (t *MeshTopology) Rehydrate(agents []db.Agent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, a := range agents {
		role := a.Role
		if role != "zone_leader" && role != "relay" {
			role = ""
		}
		parentID := ""
		if a.ParentID != nil {
			parentID = *a.ParentID
		}
		t.nodes[a.ID] = &meshNode{
			id:         a.ID,
			hostname:   a.Hostname,
			listenAddr: a.ListenAddr,
			role:       role,
			parentID:   parentID,
			children:   make(map[string]struct{}),
			online:     a.Online,
		}
		if role == "zone_leader" {
			t.zoneLeaders[a.ID] = struct{}{}
		}
	}
	// Wire children sets and depths via BFS from zone leaders.
	queue := make([]string, 0, len(t.zoneLeaders))
	for id := range t.zoneLeaders {
		if n, ok := t.nodes[id]; ok {
			n.depth = 0
			queue = append(queue, id)
		}
	}
	for i := 0; i < len(queue); i++ {
		parent := t.nodes[queue[i]]
		for id, n := range t.nodes {
			if n.parentID == parent.id {
				parent.children[id] = struct{}{}
				n.depth = parent.depth + 1
				queue = append(queue, id)
			}
		}
	}
}
