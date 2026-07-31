// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"runtime"
	"runtime/debug"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// SetBuildInfo records the running build's identity once at startup.
// Backend is "sqlite" or "postgres".  Version comes from the module's
// VCS tag when built with -trimpath -ldflags, or "(devel)" otherwise.
func SetBuildInfo(backend string) {
	version := "(devel)"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}
	metricBuildInfo.WithLabelValues(version, backend, runtime.Version()).Set(1)
}

// All Prometheus metrics live here so the names and label schemas are
// reviewable in one place.  Two families:
//
//   1. dirq self-health: counts, durations, and gauges that describe
//      how the server itself is doing — useful for SREs running dirq
//      and for alerting on degraded behavior.
//
//   2. dirq_fleet_*: aggregated views of the managed fleet, sliced by
//      collected facts (OS, CPU bucket, etc.).  The aggregator runs
//      on a 30 s tick (see metrics_fleet.go) so each /metrics scrape
//      is cheap.
//
// Naming conventions:
//   - All metrics are prefixed dirq_.
//   - Counters end in _total.
//   - Histograms end in _seconds (with bucketed _bucket / _sum / _count
//     auto-emitted by the client lib).
//   - Free Go runtime metrics (go_goroutines, go_memstats_*, etc.) are
//     registered automatically by promauto.

var (
	// ─────────────────────────────────────────────────────────
	// Build info
	// ─────────────────────────────────────────────────────────

	metricBuildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dirq_build_info",
		Help: "Build metadata for the running dirq-server binary. Value is always 1.",
	}, []string{"version", "backend", "go_version"})

	// ─────────────────────────────────────────────────────────
	// Mesh shape (gauges; refreshed by the fleet aggregator)
	// ─────────────────────────────────────────────────────────

	metricAgentsTotal = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "dirq_agents_total",
		Help: "Total number of registered agents (online + offline).",
	})

	metricAgentsOnline = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "dirq_agents_online",
		Help: "Number of agents currently considered online by the server.",
	})

	metricZoneLeaders = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "dirq_zone_leaders",
		Help: "Number of agents with a direct gRPC stream to the server.",
	})

	metricTreeDepthMax = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "dirq_tree_depth_max",
		Help: "Maximum depth observed in the relay tree (0 = no agents, 1 = ZL only, 2 = ZL + children, ...).",
	})

	metricSubtreeSize = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dirq_subtree_size",
		Help: "Number of agents in each zone leader's subtree (including the ZL itself).",
	}, []string{"zone_leader"})

	// ─────────────────────────────────────────────────────────
	// Reboot-aware placement (reliability signals)
	// ─────────────────────────────────────────────────────────

	metricAgentsProbation = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "dirq_agents_on_probation",
		Help: "Number of agents currently on reboot probation (decayed flap score >= threshold).",
	})

	metricFailureDomainsHot = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "dirq_failure_domains_hot",
		Help: "Number of failure domains currently flagged as correlated-hot (>= DomainFlapMinNodes flapping members).",
	})

	metricOrphanReassign = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "dirq_orphan_reassign_total",
		Help: "Orphaned children re-homed after a parent dropped, by action taken.",
	}, []string{"action"}) // action: "reparent" | "promote"

	// ─────────────────────────────────────────────────────────
	// Broadcast dispatcher (counters + histograms)
	// ─────────────────────────────────────────────────────────

	metricInflightSessions = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dirq_inflight_sessions",
		Help: "Number of in-flight broadcast sessions, by kind.",
	}, []string{"kind"})

	metricInflightPendingTargets = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dirq_inflight_pending_targets",
		Help: "Sum of unaccounted target agents across in-flight broadcast sessions, by kind.",
	}, []string{"kind"})

	metricBroadcastTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "dirq_broadcast_total",
		Help: "Broadcast dispatcher completions by kind and outcome (complete, incomplete, hard_timeout, canceled).",
	}, []string{"kind", "outcome"})

	metricBroadcastDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dirq_broadcast_duration_seconds",
		Help:    "Wall-clock duration of broadcast dispatcher sessions, by kind.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 180, 600},
	}, []string{"kind"})

	metricBroadcastMissingTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "dirq_broadcast_missing_total",
		Help: "Sum of agents that failed to respond across completed broadcasts, by kind. Use with dirq_broadcast_total to compute did-not-reply rate.",
	}, []string{"kind"})

	// ─────────────────────────────────────────────────────────
	// Registration + mesh churn
	// ─────────────────────────────────────────────────────────

	metricRegisterTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "dirq_register_total",
		Help: "Agent Register RPC outcomes (ok, rejected_secret, rejected_other).",
	}, []string{"outcome"})

	metricRegisterDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "dirq_register_duration_seconds",
		Help:    "Duration of agent Register RPCs.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	})

	metricPeerDisconnectTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "dirq_peer_disconnect_total",
		Help: "Total PeerDisconnected events received from relays (a relay detected a child drop).",
	})

	metricPeerConnectTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "dirq_peer_connect_total",
		Help: "Total PeerConnected events received from relays (a relay accepted a new child attachment).",
	})

	// ─────────────────────────────────────────────────────────
	// Fact cache pipeline
	// ─────────────────────────────────────────────────────────

	metricFactStageDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "dirq_fact_stage_depth",
		Help: "Number of distinct (agent_id, module) keys currently staged for flush.",
	})

	metricFactFlushTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "dirq_fact_flush_total",
		Help: "Fact-cache flush operations by backend (sqlite, postgres) and outcome (ok, error).",
	}, []string{"backend", "outcome"})

	metricFactFlushDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dirq_fact_flush_duration_seconds",
		Help:    "Duration of fact-cache flush operations, by backend.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"backend"})

	// ─────────────────────────────────────────────────────────
	// TLS
	// ─────────────────────────────────────────────────────────

	metricServerCertExpirySeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "dirq_server_cert_expiry_seconds",
		Help: "Seconds until the server's TLS certificate expires. Negative means expired. Useful for alerting (e.g. < 7d).",
	})

	// ─────────────────────────────────────────────────────────
	// Fleet composition (refreshed by metrics_fleet.go)
	// ─────────────────────────────────────────────────────────

	metricFleetCount = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dirq_fleet_count",
		Help: "Number of registered agents grouped by collected facts. Use sum by(<label>)(dirq_fleet_count) in PromQL to project on any dimension. Major distro_version only (drop minor to bound cardinality); minor version live in the agents/agent_facts tables for ad-hoc SQL queries.",
	}, []string{
		"os",               // linux, windows, ...
		"distro",           // rhel, fedora, ubuntu, ...
		"distro_version",   // major version only (8, 9, ...)
		"arch",             // amd64, arm64, ...
		"cores_bucket",     // 1, 2, 3-4, 5-8, 9-16, 17-32, 33-64, 65+
		"memory_gb_bucket", // <2, 2-4, 5-8, 9-16, 17-32, 33-64, 65-128, 129+
		"exec_enabled",     // true, false
		"online",           // true, false
	})
)
