// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"log/slog"
	"testing"
	"time"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// The reboot-aware Config knobs use a negative sentinel for "unset". Verify
// that -1 keeps the built-in default while an explicit 0 is honored (0 must
// mean "disable", NOT "fall back to default" — that's the whole reason the
// mapping guards on >= 0 instead of > 0).
func TestConfigMapping_RebootKnobSentinels(t *testing.T) {
	def := DefaultTopologyConfig()

	// All sentinels: topoCfg should equal the defaults.
	unset := New(Config{
		FlapWindow: -1, FlapThreshold: -1, ProbationChildCap: -1,
		FailureDomainPrefixV4: -1, FailureDomainPrefixV6: -1, DomainFlapMinNodes: -1,
	}, &mockDB{}, slog.Default())
	if unset.topoCfg.FlapThreshold != def.FlapThreshold {
		t.Errorf("unset FlapThreshold = %v, want default %v", unset.topoCfg.FlapThreshold, def.FlapThreshold)
	}
	if unset.topoCfg.DomainFlapMinNodes != def.DomainFlapMinNodes {
		t.Errorf("unset DomainFlapMinNodes = %d, want default %d", unset.topoCfg.DomainFlapMinNodes, def.DomainFlapMinNodes)
	}

	// Explicit 0 (disable) must override the default.
	off := New(Config{
		FlapWindow: 0, FlapThreshold: 0, ProbationChildCap: 0,
		FailureDomainPrefixV4: -1, FailureDomainPrefixV6: -1, DomainFlapMinNodes: 0,
	}, &mockDB{}, slog.Default())
	if off.topoCfg.FlapThreshold != 0 {
		t.Errorf("explicit FlapThreshold=0 not honored: got %v", off.topoCfg.FlapThreshold)
	}
	if off.topoCfg.DomainFlapMinNodes != 0 {
		t.Errorf("explicit DomainFlapMinNodes=0 not honored: got %d", off.topoCfg.DomainFlapMinNodes)
	}

	// A concrete override is applied verbatim.
	custom := New(Config{
		FlapWindow: 2 * time.Hour, FlapThreshold: 3, ProbationChildCap: 4,
		FailureDomainPrefixV4: 16, FailureDomainPrefixV6: 48, DomainFlapMinNodes: 5,
	}, &mockDB{}, slog.Default())
	if custom.topoCfg.FlapWindow != 2*time.Hour || custom.topoCfg.FailureDomainPrefixV4 != 16 || custom.topoCfg.DomainFlapMinNodes != 5 {
		t.Errorf("custom override not applied: %+v", custom.topoCfg)
	}
}

// batcherTestServer builds a Server with just enough wired up for the
// registration batcher's role-assignment path (topology + config + logger).
func batcherTestServer(cfg TopologyConfig) *Server {
	return &Server{
		topoCfg:  cfg,
		topology: NewMeshTopology(cfg),
		log:      slog.Default(),
	}
}

// Two hosts in the SAME failure domain but on distinct IPs, plus one host in a
// different domain, with exactly two open ZL slots.  The old exact-IP-only
// logic would crown both same-domain hosts (2 ZLs in one rack); the
// domain-aware two-pass greedy must instead spread the two leaders across the
// two distinct domains and demote the second same-domain host to a relay.
func TestBatcherSpreadsZoneLeadersAcrossDomains(t *testing.T) {
	cfg := DefaultTopologyConfig()
	cfg.MaxZoneLeaders = 2
	cfg.FailureDomainPrefixV4 = 24
	srv := batcherTestServer(cfg)
	b := newRegistrationBatcher(srv)

	// x1, x2 share 10.0.5.0/24; y1 is in 10.0.6.0/24.
	batch := []*pendingReg{
		{agentID: "x1", hostname: "x1", listenAddr: "10.0.5.1:9000", sourceIP: "10.0.5.1"},
		{agentID: "x2", hostname: "x2", listenAddr: "10.0.5.2:9000", sourceIP: "10.0.5.2"},
		{agentID: "y1", hostname: "y1", listenAddr: "10.0.6.1:9000", sourceIP: "10.0.6.1"},
	}
	for _, p := range batch {
		srv.topology.AddAgent(p.agentID, p.hostname, p.listenAddr)
	}

	out := b.assignBatchDiverse(batch)

	roleOf := func(i int) pb.AgentRole { return out[i].Role }
	zl := pb.AgentRole_AGENT_ROLE_ZONE_LEADER
	relay := pb.AgentRole_AGENT_ROLE_RELAY

	// Exactly two zone leaders, and they must span two distinct domains.
	zlDomains := map[string]bool{}
	zlCount := 0
	for i, p := range batch {
		if roleOf(i) == zl {
			zlCount++
			zlDomains[srv.topology.domainOf(p.listenAddr)] = true
		}
	}
	if zlCount != 2 {
		t.Fatalf("expected 2 zone leaders, got %d (%v)", zlCount, out)
	}
	if len(zlDomains) != 2 {
		t.Fatalf("expected ZLs spread across 2 domains, got %d: %v", len(zlDomains), zlDomains)
	}
	// y1 (the lone host in its domain) must be one of the leaders; one of the
	// two same-domain hosts must be demoted to relay.
	if roleOf(2) != zl {
		t.Errorf("expected y1 (distinct domain) to be a zone leader, got %v", roleOf(2))
	}
	if (roleOf(0) == zl) == (roleOf(1) == zl) {
		t.Errorf("expected exactly one of x1/x2 to be ZL and the other a relay, got x1=%v x2=%v", roleOf(0), roleOf(1))
	}
	if roleOf(0) != relay && roleOf(1) != relay {
		t.Errorf("expected one same-domain host demoted to relay, got x1=%v x2=%v", roleOf(0), roleOf(1))
	}
}

// A just-rebooted (personally flaky) host must not be crowned zone leader when
// a stable host from the same source is available in the batch.
func TestBatcherSkipsFlakyZoneLeader(t *testing.T) {
	cfg := DefaultTopologyConfig()
	cfg.MaxZoneLeaders = 1
	srv := batcherTestServer(cfg)
	b := newRegistrationBatcher(srv)

	// "flaky" has already rebooted twice; "fresh" is new. Both share a source
	// IP group so only one can be promoted.
	srv.topology.AddAgent("flaky", "flaky", "10.0.5.1:9000")
	srv.topology.MarkOffline("flaky")
	srv.topology.AddAgent("flaky", "flaky", "10.0.5.1:9000")
	srv.topology.MarkOffline("flaky")
	srv.topology.AddAgent("flaky", "flaky", "10.0.5.1:9000")
	if !srv.topology.IsFlaky("flaky") {
		t.Fatal("precondition: flaky should be on probation")
	}
	srv.topology.AddAgent("fresh", "fresh", "10.0.5.1:9000")

	batch := []*pendingReg{
		{agentID: "flaky", hostname: "flaky", listenAddr: "10.0.5.1:9000", sourceIP: "10.0.5.1"},
		{agentID: "fresh", hostname: "fresh", listenAddr: "10.0.5.1:9000", sourceIP: "10.0.5.1"},
	}
	out := b.assignBatchDiverse(batch)

	if out[0].Role == pb.AgentRole_AGENT_ROLE_ZONE_LEADER {
		t.Errorf("flaky host must not be promoted to zone leader: %v", out[0])
	}
	if out[1].Role != pb.AgentRole_AGENT_ROLE_ZONE_LEADER {
		t.Errorf("stable host should have taken the ZL slot: %v", out[1])
	}
}
