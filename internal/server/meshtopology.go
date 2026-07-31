// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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

	// now returns the current time; overridable in tests so the flap-score
	// decay is deterministic.  Defaults to time.Now.
	now func() time.Time
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
	depth      int // 0 for zone leaders; parent.depth + 1 otherwise
	online     bool

	// flapScore is a time-decayed count of this node's disappear→reappear
	// events (reboots / dropped streams).  lastFlap is when it was last
	// bumped.  Read the decayed value with currentFlapLocked — never
	// flapScore directly.  A high score means "recently unstable"; the
	// placement code keeps such nodes near the leaves.
	flapScore float64
	lastFlap  time.Time

	// domain is the node's failure domain (network-prefix bucket of its
	// listen address) — a proxy for the rack / subnet / hypervisor it shares
	// fate with.  Derived from listenAddr at registration; used to spot
	// correlated reboots across neighbours.
	domain string
}

// NewMeshTopology returns an empty topology.
func NewMeshTopology(cfg TopologyConfig) *MeshTopology {
	return &MeshTopology{
		nodes:       make(map[string]*meshNode),
		zoneLeaders: make(map[string]struct{}),
		cfg:         cfg,
		now:         time.Now,
	}
}

// currentFlapLocked returns n's flap score decayed to the present.  Reboots
// that have stopped happening fade out as exp(-elapsed/FlapWindow), so a node
// that was flaky an hour ago but has been stable since reads as reliable
// again.  Caller holds the lock.
func (t *MeshTopology) currentFlapLocked(n *meshNode) float64 {
	if n.flapScore == 0 {
		return 0
	}
	if t.cfg.FlapWindow <= 0 {
		return n.flapScore // decay disabled — score only accumulates
	}
	elapsed := t.now().Sub(n.lastFlap)
	if elapsed <= 0 {
		return n.flapScore
	}
	return n.flapScore * math.Exp(-elapsed.Seconds()/t.cfg.FlapWindow.Seconds())
}

// isFlakyLocked reports whether n is PERSONALLY on reboot probation — its own
// flap score has crossed the threshold.  This is the O(1) per-node signal;
// the correlated failure-domain signal is layered on top in the selection
// functions (see hotDomainsLocked).  Caller holds the lock.
func (t *MeshTopology) isFlakyLocked(n *meshNode) bool {
	if t.cfg.FlapThreshold <= 0 {
		return false // reliability-aware placement disabled
	}
	return t.currentFlapLocked(n) >= t.cfg.FlapThreshold
}

// domainOf buckets a listen address into a failure domain by masking it to
// the configured network prefix.  Unparseable addresses become their own
// singleton domain (conservative — they correlate with nothing).  An empty
// address yields "" (no domain).
func (t *MeshTopology) domainOf(listenAddr string) string {
	if listenAddr == "" {
		return ""
	}
	host := listenAddr
	if i := indexLastColon(listenAddr); i >= 0 {
		host = listenAddr[:i]
	}
	host = strings.Trim(host, "[]") // strip IPv6 brackets: [::1]:9000
	ip := net.ParseIP(host)
	if ip == nil {
		return host // not an IP — treat the raw host as its own domain
	}
	if v4 := ip.To4(); v4 != nil {
		bits := t.cfg.FailureDomainPrefixV4
		if bits <= 0 || bits > 32 {
			bits = 32
		}
		return v4.Mask(net.CIDRMask(bits, 32)).String() + "/" + strconv.Itoa(bits)
	}
	bits := t.cfg.FailureDomainPrefixV6
	if bits <= 0 || bits > 128 {
		bits = 128
	}
	return ip.Mask(net.CIDRMask(bits, 128)).String() + "/" + strconv.Itoa(bits)
}

// hotDomainsLocked returns the set of failure domains that currently hold at
// least DomainFlapMinNodes individually-flaky members — i.e. domains where a
// correlated reboot appears to be underway.  A single noisy host can't mark
// its domain hot; it takes MinNodes of them, so one flapper doesn't tar its
// neighbours.  One O(N) pass; callers that already scan every node fold this
// in for free.  Returns nil when the feature is disabled.  Caller holds the
// lock.
func (t *MeshTopology) hotDomainsLocked() map[string]bool {
	if t.cfg.DomainFlapMinNodes <= 0 || t.cfg.FlapThreshold <= 0 {
		return nil
	}
	counts := make(map[string]int)
	for _, n := range t.nodes {
		if n.domain != "" && t.isFlakyLocked(n) {
			counts[n.domain]++
		}
	}
	var hot map[string]bool
	for d, c := range counts {
		if c >= t.cfg.DomainFlapMinNodes {
			if hot == nil {
				hot = make(map[string]bool)
			}
			hot[d] = true
		}
	}
	return hot
}

// capacityLocked returns how many children n is currently allowed to hold.
// A node on reboot probation is capped at ProbationChildCap (default 0), so
// it never accumulates a subtree while it's likely to reboot again.  Caller
// holds the lock.
func (t *MeshTopology) capacityLocked(n *meshNode) int {
	if t.isFlakyLocked(n) && t.cfg.ProbationChildCap < t.cfg.MaxChildrenPerNode {
		return t.cfg.ProbationChildCap
	}
	return t.cfg.MaxChildrenPerNode
}

// IsFlaky reports whether the agent is currently on reboot probation.  Used
// by the registration batcher to avoid crowning a repeat-rebooter a zone
// leader.  Unknown agents are treated as reliable (false).
func (t *MeshTopology) IsFlaky(id string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n, ok := t.nodes[id]
	if !ok {
		return false
	}
	return t.isFlakyLocked(n)
}

// ReliabilityStats is a snapshot of the reboot-aware placement signals,
// exposed for metrics / debug.
type ReliabilityStats struct {
	OnProbation int // agents whose personal flap score is over the threshold
	HotDomains  int // failure domains with a correlated reboot underway
}

// ReliabilitySnapshot counts agents on personal probation and correlated-hot
// failure domains in a single locked pass.
func (t *MeshTopology) ReliabilitySnapshot() ReliabilityStats {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var stats ReliabilityStats
	counts := make(map[string]int)
	for _, n := range t.nodes {
		if t.isFlakyLocked(n) {
			stats.OnProbation++
			if n.domain != "" {
				counts[n.domain]++
			}
		}
	}
	if t.cfg.DomainFlapMinNodes > 0 {
		for _, c := range counts {
			if c >= t.cfg.DomainFlapMinNodes {
				stats.HotDomains++
			}
		}
	}
	return stats
}

// AddAgent registers an agent's identity/listen address without assigning
// a role yet.  Idempotent — refreshes listen_addr/hostname if already present.
func (t *MeshTopology) AddAgent(id, hostname, listenAddr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n, ok := t.nodes[id]; ok {
		n.hostname = hostname
		n.listenAddr = listenAddr
		n.domain = t.domainOf(listenAddr)
		if !n.online {
			// The agent had been marked offline and is now back — a reboot
			// or a stream that dropped and re-registered.  Reboots cluster
			// in time, so bump the decayed flap score.  Parent selection and
			// ZL promotion read this to keep a repeat-offender near the
			// leaves until it goes quiet long enough for the score to decay.
			n.flapScore = t.currentFlapLocked(n) + 1
			n.lastFlap = t.now()
		}
		n.online = true
		return
	}
	t.nodes[id] = &meshNode{
		id:         id,
		hostname:   hostname,
		listenAddr: listenAddr,
		domain:     t.domainOf(listenAddr),
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
	if len(p.children) >= t.capacityLocked(p) {
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

// FindShallowestParentWithRoom picks the best online node to hang a new
// child under.  Candidates are ranked by, in order:
//
//  1. Reliability — a node NOT on reboot probation is always preferred over
//     one that is.  A parent's failure orphans its whole subtree, so we keep
//     subtrees off recently-flapping nodes and only fall back to a flaky
//     parent when no stable node has room.  "On probation" here means either
//     the node is personally flapping OR it sits in a failure domain where a
//     correlated reboot is underway (hotDomainsLocked).  The personal signal
//     is a hard cap; the domain signal is a soft deprioritization only.
//  2. Depth — shallowest first (the original BFS-fill behaviour), to keep the
//     tree wide and shallow.
//  3. Load — least children first, to spread fan-out evenly.
//
// A node on probation is additionally held to ProbationChildCap children
// (default 0 → it is skipped entirely and stays a leaf).  This is an
// in-memory pass, O(N) at worst but trivially fast because everything lives
// in maps.
//
// Returns ok=false if no candidate exists (tree saturated).
func (t *MeshTopology) FindShallowestParentWithRoom() (id, listenAddr string, ok bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	hot := t.hotDomainsLocked()
	bestFlaky := false
	bestDepth := -1
	bestCount := 0
	bestID := ""
	bestAddr := ""
	for nodeID, n := range t.nodes {
		if !n.online || n.listenAddr == "" || n.role == "" {
			continue
		}
		c := len(n.children)
		if c >= t.capacityLocked(n) {
			continue
		}
		// Personal probation OR a correlated reboot in this node's domain
		// both make it a poor parent; the domain signal never blocks the
		// capacity check above, it only loses the ranking tie-break here.
		flaky := t.isFlakyLocked(n) || hot[n.domain]

		better := false
		switch {
		case bestID == "":
			better = true
		case flaky != bestFlaky:
			better = !flaky // prefer a stable parent over a flaky one
		case n.depth != bestDepth:
			better = n.depth < bestDepth
		default:
			better = c < bestCount
		}
		if better {
			bestID = nodeID
			bestAddr = n.listenAddr
			bestFlaky = flaky
			bestDepth = n.depth
			bestCount = c
		}
	}
	if bestID == "" {
		return "", "", false
	}
	return bestID, bestAddr, true
}

// FindFallbackParents picks up to `count` alternate parents for fault
// isolation, preferring ones on a different zone-leader branch AND in a
// different failure domain than primaryID.  Returns (id, listenAddr) pairs.
func (t *MeshTopology) FindFallbackParents(primaryID string, count int) []db.Agent {
	t.mu.RLock()
	defer t.mu.RUnlock()

	primaryZL := t.zoneLeaderOfLocked(primaryID)
	primaryDomain := ""
	if p, ok := t.nodes[primaryID]; ok {
		primaryDomain = p.domain
	}
	hot := t.hotDomainsLocked()

	type candidate struct {
		id         string
		listenAddr string
		hostname   string
		depth      int
		children   int
		zl         string
		sameDomain bool
		flaky      bool
	}
	var cands []candidate
	for nodeID, n := range t.nodes {
		if nodeID == primaryID || !n.online || n.listenAddr == "" || n.role == "" {
			continue
		}
		if len(n.children) >= t.capacityLocked(n) {
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
			sameDomain: primaryDomain != "" && n.domain == primaryDomain,
			flaky:      t.isFlakyLocked(n) || hot[n.domain],
		})
	}

	// Prefer, in order: a different ZL branch (subtree fault isolation); a
	// different failure domain than the primary (rack/power fault isolation —
	// a fallback in the same rack is useless when the rack is what dropped);
	// a stable node over a flaky one (a fallback is used precisely when the
	// primary just failed — don't fail over onto another repeat-rebooter or
	// into a domain that's already churning); then shallowest; then least
	// loaded.
	sort.Slice(cands, func(i, j int) bool {
		ai, aj := cands[i], cands[j]
		iDiff := ai.zl != primaryZL
		jDiff := aj.zl != primaryZL
		if iDiff != jDiff {
			return iDiff
		}
		if ai.sameDomain != aj.sameDomain {
			return !ai.sameDomain // different domain first
		}
		if ai.flaky != aj.flaky {
			return !ai.flaky
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
	FlapScore  float64 // decayed reboot-propensity score (0 = never flapped)
	Flaky      bool    // true when personally on reboot probation (FlapScore >= threshold)
	Domain     string  // failure domain (network-prefix bucket of ListenAddr)
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
		FlapScore:  t.currentFlapLocked(n),
		Flaky:      t.isFlakyLocked(n),
		Domain:     n.domain,
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
//
// Flap history is intentionally NOT restored: it's soft, in-memory-only
// reliability state.  After a server restart every agent re-registers, and
// a genuinely flaky node will re-accumulate a flap score within a window or
// two.  Starting everyone at score 0 just means the first post-restart
// placement is reboot-blind, which is acceptable.
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
			domain:     t.domainOf(a.ListenAddr),
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
