// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/atgreen/dirq/internal/db"
	"github.com/prometheus/client_golang/prometheus"
)

// x509ParseCert is a thin wrapper so tests can stub if needed.
var x509ParseCert = x509.ParseCertificate

// fleetMetricsRefresh recomputes mesh-shape gauges (counts, ZL fan-out,
// tree depth) and the per-(os, distro, version, arch, cores, memory)
// dirq_fleet_count gauge.  Called on a 30 s ticker so a single
// /metrics scrape doesn't trigger a full ListAgents + LoadFacts walk
// every time Prometheus scrapes (default 15 s).
//
// Cost at 50k agents: one ListAgents (full table), three
// GetFactsByModule reads (os_info, cpu, memory).  Postgres handles
// this fine; SQLite on a single writer is the more interesting one
// to watch, but read traffic is concurrent with writes and the fact
// rows are typically a few KB each — well inside the budget for a
// 30 s tick.

const fleetMetricsRefreshInterval = 30 * time.Second

// startFleetMetricsRefresher runs the periodic recompute loop until ctx
// is canceled.  Idempotent — called once from Server.Start.
func (s *Server) startFleetMetricsRefresher(ctx context.Context) {
	go func() {
		// Run once at startup so /metrics has data before the first tick.
		s.refreshFleetMetrics(ctx)
		ticker := time.NewTicker(fleetMetricsRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshFleetMetrics(ctx)
			}
		}
	}()
}

// fleetCountKey is the label tuple for dirq_fleet_count.  The fields
// match the metric's label order.
type fleetCountKey struct {
	OS             string
	Distro         string
	DistroVersion  string // major only (e.g. "8" not "8.10")
	Arch           string
	CoresBucket    string
	MemoryGBBucket string
	ExecEnabled    string
	Online         string
}

var (
	// Track which fleetCountKey series we've emitted so we can delete
	// stale series on the next refresh (host count drops to 0, fleet
	// shrinks).  Without this, Prometheus would keep reporting last-
	// known values indefinitely for vanished combinations.
	prevFleetSeries   = map[fleetCountKey]struct{}{}
	prevFleetSeriesMu sync.Mutex

	prevSubtreeSeries   = map[string]struct{}{}
	prevSubtreeSeriesMu sync.Mutex
)

func (s *Server) refreshFleetMetrics(ctx context.Context) {
	agents, err := s.db.ListAgents(ctx, db.ListAgentsFilter{})
	if err != nil {
		s.log.Warn("metrics: ListAgents failed", "error", err)
		return
	}

	// Aggregate online/total + per-ZL subtree counts from the in-memory
	// topology (single read pass under the topology lock).
	var total, online int
	for _, a := range agents {
		total++
		if a.Online {
			online++
		}
	}
	metricAgentsTotal.Set(float64(total))
	metricAgentsOnline.Set(float64(online))

	// Mesh shape: zone leaders, depth, per-ZL subtree sizes.
	zlIDs := s.topology.ZoneLeaderIDs()
	metricZoneLeaders.Set(float64(len(zlIDs)))

	maxDepth := 0
	currentSubtree := make(map[string]struct{}, len(zlIDs))
	zlHostnameByID := map[string]string{}
	for _, a := range agents {
		if a.Role == "zone_leader" {
			zlHostnameByID[a.ID] = a.Hostname
		}
		if n, ok := s.topology.Get(a.ID); ok && n.Depth > maxDepth {
			maxDepth = n.Depth
		}
	}
	metricTreeDepthMax.Set(float64(maxDepth))

	// Reboot-aware placement signals.
	relStats := s.topology.ReliabilitySnapshot()
	metricAgentsProbation.Set(float64(relStats.OnProbation))
	metricFailureDomainsHot.Set(float64(relStats.HotDomains))

	for _, zlID := range zlIDs {
		size := len(s.topology.SubtreeIDs(zlID))
		label := zlHostnameByID[zlID]
		if label == "" {
			label = zlID
		}
		metricSubtreeSize.WithLabelValues(label).Set(float64(size))
		currentSubtree[label] = struct{}{}
	}
	// Drop ZLs that have disappeared.
	prevSubtreeSeriesMu.Lock()
	for label := range prevSubtreeSeries {
		if _, ok := currentSubtree[label]; !ok {
			metricSubtreeSize.DeleteLabelValues(label)
		}
	}
	prevSubtreeSeries = currentSubtree
	prevSubtreeSeriesMu.Unlock()

	// Fact-stage depth (cheap — under the same lock the writer uses).
	s.factStageMu.Lock()
	stageDepth := len(s.factStage)
	s.factStageMu.Unlock()
	metricFactStageDepth.Set(float64(stageDepth))

	// Pending targets per session kind — sum of Remaining() across all
	// in-flight sessions.  dirq_inflight_sessions is incremented at
	// dispatcher entry; this gauge is the more useful "is anything
	// stuck" signal.
	var qPending, ePending, dPending int
	querySessionsMu.RLock()
	for _, qs := range querySessions {
		qPending += qs.Remaining()
	}
	querySessionsMu.RUnlock()
	execBroadcastSessionsMu.RLock()
	for _, bs := range execBroadcastSessions {
		ePending += bs.Remaining()
	}
	execBroadcastSessionsMu.RUnlock()
	deploySessionsMu.RLock()
	for _, ds := range deploySessions {
		dPending += ds.Remaining()
	}
	deploySessionsMu.RUnlock()
	metricInflightPendingTargets.WithLabelValues("query").Set(float64(qPending))
	metricInflightPendingTargets.WithLabelValues("exec").Set(float64(ePending))
	metricInflightPendingTargets.WithLabelValues("deploy").Set(float64(dPending))

	// Fleet composition: load os_info, cpu, memory keyed by agent_id.
	osInfo := factsByAgent(ctx, s.db, "os_info", s.log)
	cpuInfo := factsByAgent(ctx, s.db, "cpu", s.log)
	memInfo := factsByAgent(ctx, s.db, "memory", s.log)

	counts := map[fleetCountKey]int{}
	for _, a := range agents {
		k := fleetCountKey{
			OS:             nonEmpty(a.OS, "unknown"),
			Distro:         nonEmpty(stringField(osInfo[a.ID], "distro"), "unknown"),
			DistroVersion:  majorVersion(stringField(osInfo[a.ID], "distro_version"), a.OSVersion),
			Arch:           nonEmpty(a.Arch, "unknown"),
			CoresBucket:    coresBucket(intField(cpuInfo[a.ID], "logical_cores")),
			MemoryGBBucket: memoryGBBucket(intField(memInfo[a.ID], "total_bytes")),
			ExecEnabled:    strconv.FormatBool(a.ExecEnabled),
			Online:         strconv.FormatBool(a.Online),
		}
		counts[k]++
	}

	// Emit gauges; drop series that vanished since last refresh.
	prevFleetSeriesMu.Lock()
	defer prevFleetSeriesMu.Unlock()
	current := make(map[fleetCountKey]struct{}, len(counts))
	for k, c := range counts {
		metricFleetCount.With(prometheus.Labels{
			"os":               k.OS,
			"distro":           k.Distro,
			"distro_version":   k.DistroVersion,
			"arch":             k.Arch,
			"cores_bucket":     k.CoresBucket,
			"memory_gb_bucket": k.MemoryGBBucket,
			"exec_enabled":     k.ExecEnabled,
			"online":           k.Online,
		}).Set(float64(c))
		current[k] = struct{}{}
	}
	for k := range prevFleetSeries {
		if _, ok := current[k]; !ok {
			metricFleetCount.Delete(prometheus.Labels{
				"os":               k.OS,
				"distro":           k.Distro,
				"distro_version":   k.DistroVersion,
				"arch":             k.Arch,
				"cores_bucket":     k.CoresBucket,
				"memory_gb_bucket": k.MemoryGBBucket,
				"exec_enabled":     k.ExecEnabled,
				"online":           k.Online,
			})
		}
	}
	prevFleetSeries = current
}

// ─────────────────────────────────────────────────────────
// Helpers — fact unpacking + label bucketing
// ─────────────────────────────────────────────────────────

// factsByAgent loads all rows for a module and returns them keyed by
// agent ID.  Each row's Data is parsed once into a generic map for
// stringField / intField extraction.  On any error, returns an empty
// map and logs — metrics emission proceeds with whatever's available.
func factsByAgent(ctx context.Context, dbh db.DB, module string, log Logger) map[string]map[string]any {
	rows, err := dbh.GetFactsByModule(ctx, module)
	if err != nil {
		log.Warn("metrics: GetFactsByModule failed", "module", module, "error", err)
		return map[string]map[string]any{}
	}
	out := make(map[string]map[string]any, len(rows))
	for _, r := range rows {
		var m map[string]any
		if err := json.Unmarshal(r.Data, &m); err == nil {
			out[r.AgentID] = m
		}
	}
	return out
}

// Logger is the minimal slog-compatible surface needed here so the file
// stays trivially testable.  Server.log satisfies it.
type Logger interface {
	Warn(msg string, args ...any)
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func intField(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// majorVersion extracts the leading numeric component of a version
// string so cardinality stays bounded ("8.10" -> "8", "9.4.1" -> "9").
// Falls back to the agent-reported os_version if the fact value is
// empty.  Returns "unknown" if neither is parseable.
func majorVersion(factVer, fallbackVer string) string {
	pick := factVer
	if pick == "" {
		pick = fallbackVer
	}
	if pick == "" {
		return "unknown"
	}
	for i, r := range pick {
		if r == '.' || r == '-' || r == '_' {
			if i == 0 {
				return "unknown"
			}
			return pick[:i]
		}
	}
	return pick
}

func coresBucket(n int64) string {
	switch {
	case n <= 0:
		return "unknown"
	case n == 1:
		return "1"
	case n == 2:
		return "2"
	case n <= 4:
		return "3-4"
	case n <= 8:
		return "5-8"
	case n <= 16:
		return "9-16"
	case n <= 32:
		return "17-32"
	case n <= 64:
		return "33-64"
	default:
		return "65+"
	}
}

// startServerCertExpiryRefresher updates dirq_server_cert_expiry_seconds
// every minute.  Cheap — reads the current cert from the in-memory
// reloader and computes (NotAfter - now).  No-op when TLS is disabled.
func (s *Server) startServerCertExpiryRefresher(ctx context.Context) {
	if s.certReloader == nil {
		return
	}
	go func() {
		tick := func() {
			cert, err := s.certReloader.GetCertificate(nil)
			if err != nil || cert == nil || len(cert.Certificate) == 0 {
				return
			}
			leaf := cert.Leaf
			if leaf == nil {
				// Parse on demand if reloader didn't pre-populate Leaf.
				parsed, parseErr := x509ParseCert(cert.Certificate[0])
				if parseErr != nil {
					return
				}
				leaf = parsed
			}
			metricServerCertExpirySeconds.Set(time.Until(leaf.NotAfter).Seconds())
		}
		tick()
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tick()
			}
		}
	}()
}

func memoryGBBucket(bytes int64) string {
	if bytes <= 0 {
		return "unknown"
	}
	gb := bytes / (1024 * 1024 * 1024)
	switch {
	case gb < 2:
		return "<2"
	case gb <= 4:
		return "2-4"
	case gb <= 8:
		return "5-8"
	case gb <= 16:
		return "9-16"
	case gb <= 32:
		return "17-32"
	case gb <= 64:
		return "33-64"
	case gb <= 128:
		return "65-128"
	default:
		return "129+"
	}
}
