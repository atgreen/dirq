// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"sync"
	"time"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// registrationBatcher holds incoming Register RPCs in a short window
// before running role assignment, so a burst of agents arriving from
// the same VM doesn't lock the first N zone-leader slots onto whichever
// VM happened to win the race.
//
// Why it exists: the BFS-fill assigner reacts one Register at a time
// in arrival order.  Under a burst (rack reboot, post-maintenance
// recovery, fleet-scale emulation) the first N agents become zone
// leaders regardless of where they came from — and "where they came
// from" is what actually determines fault sharing.  By holding new
// registrations for ~200 ms and assigning the whole batch with full
// visibility of source IPs, the batcher can deliberately spread zone
// leaders across distinct hosts in one decision.
//
// Single registrations (steady state) flow through with one batch
// window of added latency.  Register isn't a hot path; the extra
// ~200 ms is invisible compared to gRPC + TLS handshake costs.
type registrationBatcher struct {
	mu    sync.Mutex
	queue []*pendingReg
	timer *time.Timer
	s     *Server

	// batchWindow is how long the batcher waits after the first arrival
	// before flushing.  Bursts of registrations accumulate during this
	// window; lone registrations pay it as latency.
	batchWindow time.Duration

	// batchCap forces an early flush when the queue grows past this
	// size, so very large bursts don't all sit waiting for the timer.
	batchCap int
}

// pendingReg is one in-flight registration waiting for a role
// assignment.  The caller has already created the DB record and
// inserted the agent into MeshTopology; only the (role, parent_id)
// decision remains.
type pendingReg struct {
	agentID    string
	hostname   string
	listenAddr string
	sourceIP   string // peer IP — used for ZL spread-by-host
	resp       chan assignment
}

func newRegistrationBatcher(s *Server) *registrationBatcher {
	return &registrationBatcher{
		s:           s,
		batchWindow: 200 * time.Millisecond,
		batchCap:    200,
	}
}

// submit enqueues a registration and returns the assignment when it's ready.
// Caller blocks until either the batch window elapses or the queue reaches
// batchCap and an early flush triggers.
func (b *registrationBatcher) submit(p *pendingReg) assignment {
	b.mu.Lock()
	b.queue = append(b.queue, p)
	if len(b.queue) >= b.batchCap {
		batch := b.queue
		b.queue = nil
		if b.timer != nil {
			b.timer.Stop()
			b.timer = nil
		}
		b.mu.Unlock()
		go b.flushBatch(batch)
	} else {
		if b.timer == nil {
			b.timer = time.AfterFunc(b.batchWindow, b.timerFire)
		}
		b.mu.Unlock()
	}
	return <-p.resp
}

func (b *registrationBatcher) timerFire() {
	b.mu.Lock()
	batch := b.queue
	b.queue = nil
	b.timer = nil
	b.mu.Unlock()
	if len(batch) > 0 {
		b.flushBatch(batch)
	}
}

// flushBatch assigns roles to a whole batch through the diversity-aware
// path.  An earlier version had a fast path for single-item batches
// that bypassed IP diversity — but at registration-jitter rates of
// ~1–2 arrivals per 200 ms window most batches are size 1, which meant
// the fast path won the race and stacked 4 ZLs onto whichever VM's
// VHs happened to arrive first.  No fast path now: every batch runs
// through assignBatchDiverse, where a size-1 batch from a VM that
// already has a ZL falls through to relay assignment (correct), and
// a size-1 batch from a ZL-free VM gets promoted to ZL (correct).
func (b *registrationBatcher) flushBatch(batch []*pendingReg) {
	if len(batch) > 1 {
		b.s.log.Info("registration batch flushing",
			"batch_size", len(batch),
			"distinct_source_ips", countDistinctIPs(batch),
		)
	}
	assignments := b.assignBatchDiverse(batch)
	for i, p := range batch {
		p.resp <- assignments[i]
	}
}

// assignBatchDiverse picks zone-leader candidates with source-IP
// diversity preference, then fills relay slots via the standard BFS
// path.  Result indexes correspond to the input batch order.
func (b *registrationBatcher) assignBatchDiverse(batch []*pendingReg) []assignment {
	cfg := b.s.topoCfg
	out := make([]assignment, len(batch))

	// How many fresh ZL slots are open?
	openZL := cfg.MaxZoneLeaders - b.s.topology.CountOnlineZoneLeaders()

	// Group batch indices by source IP.  Within each IP group we'll only
	// promote one agent to ZL; the rest go through relay assignment.
	type ipGroup struct {
		ip      string
		indices []int
	}
	indexByIP := make(map[string][]int)
	var ipOrder []string
	for i, p := range batch {
		ip := p.sourceIP
		if _, seen := indexByIP[ip]; !seen {
			ipOrder = append(ipOrder, ip)
		}
		indexByIP[ip] = append(indexByIP[ip], i)
	}
	groups := make([]ipGroup, 0, len(ipOrder))
	for _, ip := range ipOrder {
		groups = append(groups, ipGroup{ip: ip, indices: indexByIP[ip]})
	}

	// Skip IPs that already host a zone leader so we never stack a new
	// ZL on top of an existing one in the same source.  Track failure
	// domains too, so we can prefer spreading ZLs across racks/subnets and
	// not just distinct IPs within one rack.
	ipHasZL := make(map[string]bool)
	domainHasZL := make(map[string]bool)
	for _, zlID := range b.s.topology.ZoneLeaderIDs() {
		if n, ok := b.s.topology.Get(zlID); ok && n.Online {
			ipHasZL[ipOfListen(n.ListenAddr)] = true
			if n.Domain != "" {
				domainHasZL[n.Domain] = true
			}
		}
	}

	// Promote one rep per distinct (and ZL-free) IP up to openZL.  We
	// deliberately do NOT stack a second ZL onto an IP that already has
	// one within the same batch — the whole point of batching is to
	// spread ZLs by source IP.  If openZL remains positive after this
	// pass (e.g. the batch only saw 1 source IP but there are 4 free ZL
	// slots), leave the slots open.  Later batches that contain other
	// IPs will fill them; the rebalancer's "promote a relay with
	// children" path covers the steady-state case if no new IPs ever
	// arrive.
	// For each IP group pick a representative that is NOT on reboot
	// probation.  A zone leader is the most expensive slot to lose — its
	// entire subtree goes offline (MarkSubtreeOffline) on every reboot — so
	// we never crown a repeat-rebooter here.  rep == -1 means the whole
	// group just flapped; those groups are skipped and their agents fall
	// through to relay/leaf placement below.
	reps := make([]int, len(groups))
	domainOfGroup := make([]string, len(groups))
	for gi, g := range groups {
		reps[gi] = -1
		for _, idx := range g.indices {
			if !b.s.topology.IsFlaky(batch[idx].agentID) {
				reps[gi] = idx
				break
			}
		}
		domainOfGroup[gi] = b.s.topology.domainOf(batch[g.indices[0]].listenAddr)
	}

	promoted := make(map[int]bool)
	promote := func(gi int) {
		rep := reps[gi]
		p := batch[rep]
		b.s.topology.AssignZoneLeader(p.agentID)
		out[rep] = assignment{Role: pb.AgentRole_AGENT_ROLE_ZONE_LEADER}
		promoted[rep] = true
		ipHasZL[groups[gi].ip] = true
		if d := domainOfGroup[gi]; d != "" {
			domainHasZL[d] = true
		}
		openZL--
	}

	// Pass 1 — spread across failure domains: promote one rep per group
	// whose IP *and* failure domain are both still unrepresented.  This
	// biases the scarce ZL slots toward distinct racks/subnets first.
	if openZL > 0 {
		for gi := range groups {
			if openZL <= 0 {
				break
			}
			if reps[gi] < 0 || ipHasZL[groups[gi].ip] || domainHasZL[domainOfGroup[gi]] {
				continue
			}
			promote(gi)
		}
	}

	// Pass 2 — fill any remaining slots across still-distinct IPs, even if
	// their domain already hosts a ZL.  This preserves the original
	// exact-IP anti-stacking behaviour (a single-subnet fleet still gets up
	// to MaxZoneLeaders leaders spread over distinct hosts); domain
	// diversity is a preference layered on top, never a hard cap that would
	// starve a one-rack deployment of zone leaders.
	if openZL > 0 {
		for gi := range groups {
			if openZL <= 0 {
				break
			}
			if reps[gi] < 0 || ipHasZL[groups[gi].ip] {
				continue
			}
			promote(gi)
		}
	}

	// Everyone else becomes a relay via BFS-fill — explicitly NOT
	// promoting any of them to ZL even when openZL > 0 remains.  If the
	// batch only saw one source IP, we leave ZL slots open for future
	// batches with different IPs.  Without this restriction, the
	// fallback assignRole would happily promote same-IP candidates and
	// defeat the diversity logic above.
	for i := range batch {
		if promoted[i] {
			continue
		}
		out[i] = b.s.applyRelayAssignment(batch[i])
	}

	return out
}

// applyRelayAssignment is the relay-only commit path used by the
// multi-agent batch's non-ZL slice.  The batcher has already decided
// who gets ZL slots based on source-IP diversity; everyone else must
// be a relay even when openZL > 0 remains.  If the BFS-fill finds no
// parent with room (rare — the tree should always have room until
// saturated), this falls back to promotion as the orphan-protection
// escape hatch.
func (s *Server) applyRelayAssignment(p *pendingReg) assignment {
	parentID, parentAddr, ok := s.topology.FindShallowestParentWithRoom()
	if !ok {
		s.log.Warn("topology: no parent with room during batch relay fill, promoting",
			"agent_id", p.agentID)
		s.topology.AssignZoneLeader(p.agentID)
		return assignment{Role: pb.AgentRole_AGENT_ROLE_ZONE_LEADER}
	}
	fbAgents := s.topology.FindFallbackParents(parentID, 2)
	fallbacks := make([]string, 0, len(fbAgents))
	fallbackIDs := make([]string, 0, len(fbAgents))
	for _, fb := range fbAgents {
		fallbacks = append(fallbacks, fb.ListenAddr)
		fallbackIDs = append(fallbackIDs, fb.ID)
	}
	if !s.topology.AssignChild(p.agentID, parentID) {
		s.log.Warn("topology: chosen parent full at commit during batch relay fill, promoting",
			"agent_id", p.agentID, "intended_parent", parentID)
		s.topology.AssignZoneLeader(p.agentID)
		return assignment{Role: pb.AgentRole_AGENT_ROLE_ZONE_LEADER}
	}
	return assignment{
		Role:          pb.AgentRole_AGENT_ROLE_RELAY,
		ParentID:      parentID,
		ParentAddr:    parentAddr,
		FallbackAddrs: fallbacks,
		FallbackIDs:   fallbackIDs,
	}
}

func countDistinctIPs(batch []*pendingReg) int {
	seen := make(map[string]bool, len(batch))
	for _, p := range batch {
		seen[p.sourceIP] = true
	}
	return len(seen)
}

func ipOfListen(addr string) string {
	if i := indexLastColon(addr); i >= 0 {
		return addr[:i]
	}
	return addr
}

func indexLastColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}
