// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/atgreen/dirq/internal/db"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// inflightSession is the JSON shape returned by /api/v1/debug/inflight.
type inflightSession struct {
	RequestID         string         `json:"request_id"`
	Kind              string         `json:"kind"` // "exec", "exec_multi", "query", "put_file", "fetch_file"
	Targets           int            `json:"targets"`
	Received          int            `json:"received"`
	Missing           []string       `json:"missing,omitempty"`         // agent IDs that haven't answered
	ArrivalsLast1s    int            `json:"arrivals_last_1s,omitempty"`
	ArrivalsLast5s    int            `json:"arrivals_last_5s,omitempty"`
	ArrivalsLast30s   int            `json:"arrivals_last_30s,omitempty"`
	ByZoneLeader      []zlBreakdown  `json:"by_zone_leader,omitempty"`
	ElapsedMS         int64          `json:"elapsed_ms"`
	TimeoutMS         int64          `json:"timeout_ms"`
	StartedAt         string         `json:"started_at"`
}

// zlBreakdown attributes the pending set of one in-flight broadcast to
// the zone leader each missing agent routes through, so operators can
// see at a glance which ZL's subtree is dragging a broadcast down.
type zlBreakdown struct {
	ZoneLeaderID       string `json:"zone_leader_id"`
	ZoneLeaderHostname string `json:"zone_leader_hostname"`
	ZoneLeaderAddr     string `json:"zone_leader_addr"`
	SubtreeSize        int    `json:"subtree_size"`
	Received           int    `json:"received"`
	Pending            int    `json:"pending"`
	StreamConnected    bool   `json:"stream_connected"`
	SendBufUsed        int    `json:"send_buf_used"`
	SendBufCap         int    `json:"send_buf_cap"`
}

type inflightResponse struct {
	Sessions []inflightSession `json:"sessions"`
	Now      string            `json:"now"`
}

// buildZLBreakdown attributes the still-pending agent set to its zone
// leader and reports per-ZL pending/received/stream-buffer state.  This
// is the diagnostic that surfaces "which ZL is the bottleneck" — when
// one ZL's send_buf_used is at capacity and its pending count is high,
// you've found the chokepoint.
func (s *Server) buildZLBreakdown(missing []string, total int) []zlBreakdown {
	zlSubtreeMissing := make(map[string]int)
	for _, id := range missing {
		zlID, ok := s.topology.FindZoneLeader(id)
		if !ok {
			zlID = "" // orphan; group under empty string
		}
		zlSubtreeMissing[zlID]++
	}
	if len(zlSubtreeMissing) == 0 {
		return nil
	}

	// Sort ZLs by pending count, descending — the worst offender first.
	type entry struct {
		id      string
		pending int
	}
	entries := make([]entry, 0, len(zlSubtreeMissing))
	for id, p := range zlSubtreeMissing {
		entries = append(entries, entry{id: id, pending: p})
	}
	// Manual bubble — len is tiny.
	for i := range entries {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].pending > entries[i].pending {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}

	out := make([]zlBreakdown, 0, len(entries))
	for _, e := range entries {
		b := zlBreakdown{
			ZoneLeaderID: e.id,
			Pending:      e.pending,
		}
		if e.id == "" {
			b.ZoneLeaderHostname = "(orphan — no ZL in topology)"
		} else if n, ok := s.topology.Get(e.id); ok {
			b.ZoneLeaderHostname = n.Hostname
			b.ZoneLeaderAddr = n.ListenAddr
			b.SubtreeSize = len(s.topology.SubtreeIDs(e.id))
			b.Received = b.SubtreeSize - e.pending
		}
		s.mu.RLock()
		if as, ok := s.streams[e.id]; ok {
			b.StreamConnected = true
			b.SendBufUsed = len(as.send)
			b.SendBufCap = cap(as.send)
		}
		s.mu.RUnlock()
		out = append(out, b)
	}
	return out
}

// handleDebugInflight dumps every in-flight exec / query / file-op session
// the server is currently coordinating. Used by `dirq debug inflight` to
// answer "what is the server waiting for?" without attaching a debugger
// to the process.
func (s *Server) handleDebugInflight(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	out := inflightResponse{Now: now.UTC().Format(time.RFC3339Nano)}

	// Single-host exec / put_file / fetch_file — one target each.
	s.execMu.RLock()
	for id, es := range s.execSessions {
		out.Sessions = append(out.Sessions, inflightSession{
			RequestID: id,
			Kind:      kindFromExecID(id),
			Targets:   1,
			Received:  0, // single-host: if we're still in the map, response hasn't landed
			ElapsedMS: now.Sub(es.startedAt).Milliseconds(),
			TimeoutMS: es.timeout.Milliseconds(),
			StartedAt: es.startedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	s.execMu.RUnlock()

	// Broadcast exec.
	execBroadcastSessionsMu.RLock()
	for id, bs := range execBroadcastSessions {
		missing := bs.PendingSnapshot()
		out.Sessions = append(out.Sessions, inflightSession{
			RequestID:       id,
			Kind:            "exec_multi",
			Targets:         bs.Total(),
			Received:        bs.AccountedCount(),
			Missing:         missing,
			ArrivalsLast1s:  bs.ArrivalsSince(1 * time.Second),
			ArrivalsLast5s:  bs.ArrivalsSince(5 * time.Second),
			ArrivalsLast30s: bs.ArrivalsSince(30 * time.Second),
			ByZoneLeader:    s.buildZLBreakdown(missing, bs.Total()),
			ElapsedMS:       now.Sub(bs.startedAt).Milliseconds(),
			TimeoutMS:       bs.timeout.Milliseconds(),
			StartedAt:       bs.startedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	execBroadcastSessionsMu.RUnlock()

	// Broadcast query.
	querySessionsMu.RLock()
	for id, qs := range querySessions {
		missing := qs.PendingSnapshot()
		out.Sessions = append(out.Sessions, inflightSession{
			RequestID:       id,
			Kind:            "query",
			Targets:         qs.Total(),
			Received:        qs.AccountedCount(),
			Missing:         missing,
			ArrivalsLast1s:  qs.ArrivalsSince(1 * time.Second),
			ArrivalsLast5s:  qs.ArrivalsSince(5 * time.Second),
			ArrivalsLast30s: qs.ArrivalsSince(30 * time.Second),
			ByZoneLeader:    s.buildZLBreakdown(missing, qs.Total()),
			ElapsedMS:       now.Sub(qs.startedAt).Milliseconds(),
			TimeoutMS:       qs.timeout.Milliseconds(),
			StartedAt:       qs.startedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	querySessionsMu.RUnlock()

	// Broadcast deploy.
	deploySessionsMu.RLock()
	for id, ds := range deploySessions {
		missing := ds.PendingSnapshot()
		out.Sessions = append(out.Sessions, inflightSession{
			RequestID:       id,
			Kind:            "deploy",
			Targets:         ds.Total(),
			Received:        ds.AccountedCount(),
			Missing:         missing,
			ArrivalsLast1s:  ds.ArrivalsSince(1 * time.Second),
			ArrivalsLast5s:  ds.ArrivalsSince(5 * time.Second),
			ArrivalsLast30s: ds.ArrivalsSince(30 * time.Second),
			ByZoneLeader:    s.buildZLBreakdown(missing, ds.Total()),
			ElapsedMS:       now.Sub(ds.startedAt).Milliseconds(),
			TimeoutMS:       ds.timeout.Milliseconds(),
			StartedAt:       ds.startedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	deploySessionsMu.RUnlock()

	jsonResponse(w, http.StatusOK, out)
}

// ─────────────────────────────────────────────────────────
// /api/v1/debug/stream/{id} — server-side stream-state lookup
// ─────────────────────────────────────────────────────────

type streamStateResponse struct {
	AgentID           string `json:"agent_id"`
	Hostname          string `json:"hostname"`
	DirectlyConnected bool   `json:"directly_connected"`
	SendBufferUsed    int    `json:"send_buffer_used,omitempty"`
	SendBufferCap     int    `json:"send_buffer_cap,omitempty"`
	Reassigned        bool   `json:"reassigned,omitempty"`
	RouteVia          string `json:"route_via,omitempty"` // zone-leader agent ID, when not directly connected
	RouteViaHostname  string `json:"route_via_hostname,omitempty"`
	RouteViaConnected bool   `json:"route_via_connected,omitempty"` // is the zone leader itself in s.streams?
	Note              string `json:"note,omitempty"`
}

// handleDebugStream reports the server's in-memory view of how it would
// reach a specific agent. Distinct from /api/v1/hosts which only reports
// the DB record — this answers "do you have a live stream right now,
// and if not, which zone leader would you route through?"
func (s *Server) handleDebugStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		httpError(w, http.StatusBadRequest, "agent id or hostname is required")
		return
	}

	agent, err := s.resolveAgent(r.Context(), id)
	if err != nil {
		httpError(w, http.StatusNotFound, "agent not found")
		return
	}

	out := streamStateResponse{
		AgentID:  agent.ID,
		Hostname: agent.Hostname,
	}

	s.mu.RLock()
	as, ok := s.streams[agent.ID]
	s.mu.RUnlock()

	if ok {
		out.DirectlyConnected = true
		out.SendBufferUsed = len(as.send)
		out.SendBufferCap = cap(as.send)
		out.Reassigned = as.reassigned
		jsonResponse(w, http.StatusOK, out)
		return
	}

	// Not directly connected — figure out the zone leader the server's
	// dispatch would normally route through, and whether that zone
	// leader itself has a live stream.
	zl, ok := s.topology.FindZoneLeaderAgent(agent.ID)
	if !ok {
		out.Note = "no zone leader found in topology; single-host exec would fall back to fan-out broadcast"
		jsonResponse(w, http.StatusOK, out)
		return
	}
	out.RouteVia = zl.ID
	out.RouteViaHostname = zl.Hostname
	s.mu.RLock()
	_, zlConnected := s.streams[zl.ID]
	s.mu.RUnlock()
	out.RouteViaConnected = zlConnected
	if !zlConnected {
		out.Note = "zone leader's stream is not present at this server; dispatch falls back to fan-out broadcast"
	}
	jsonResponse(w, http.StatusOK, out)
}

// ─────────────────────────────────────────────────────────
// /api/v1/debug/ping/{id} — round-trip probe through the mesh
// ─────────────────────────────────────────────────────────

type pingResponse struct {
	AgentID      string `json:"agent_id"`
	Hostname     string `json:"hostname"`
	Success      bool   `json:"success"`
	RC           int    `json:"rc"`
	ElapsedMS    int64  `json:"elapsed_ms"`
	DispatchPath string `json:"dispatch_path"` // "direct" or "fanout"
	Error        string `json:"error,omitempty"`
}

// handleDebugPing sends a no-op exec to the named agent and reports
// round-trip timing. Cross-platform: picks `true` on Linux/macOS and
// `exit 0` on Windows. This is the only mesh-truth probe — it does NOT
// rely on the DB chain being correct; if the agent is reachable through
// any path, ping succeeds.
func (s *Server) handleDebugPing(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		httpError(w, http.StatusBadRequest, "agent id or hostname is required")
		return
	}
	timeoutSec := 10
	if t := r.URL.Query().Get("timeout"); t != "" {
		if parsed, err := strconv.Atoi(t); err == nil && parsed > 0 {
			timeoutSec = parsed
		}
	}

	agent, err := s.resolveAgent(r.Context(), id)
	if err != nil {
		httpError(w, http.StatusNotFound, "agent not found")
		return
	}
	if !agent.ExecEnabled {
		httpError(w, http.StatusForbidden, "ping requires exec_enabled on the target agent")
		return
	}

	// Cross-platform no-op. Windows agents resolve "windows" as the OS
	// field; everything else (rhel/fedora/ubuntu/darwin/...) is treated
	// as POSIX-shell-capable.
	var cmd string
	if strings.EqualFold(agent.OS, "windows") {
		cmd = "exit 0"
	} else {
		cmd = "true"
	}

	// Record whether the agent is directly connected before dispatch so
	// we can label the result as direct vs fan-out routing.
	s.mu.RLock()
	_, direct := s.streams[agent.ID]
	s.mu.RUnlock()
	path := "fanout"
	if direct {
		path = "direct"
	}

	requestID := fmt.Sprintf("ping-%d", time.Now().UnixNano())
	pbReq := &pb.ServerMessage{
		Payload: &pb.ServerMessage_ExecRequest{
			ExecRequest: &pb.ExecRequest{
				RequestId:      requestID,
				AgentId:        agent.ID,
				Command:        cmd,
				TimeoutSeconds: int32(timeoutSec),
			},
		},
	}

	// Audit the probe in the exec log so operators can trace it later.
	cmdAudit := "[dirq debug ping] " + cmd
	s.db.CreateExecLog(r.Context(), db.ExecLog{
		RequestID: requestID,
		AgentID:   agent.ID,
		Hostname:  agent.Hostname,
		Operation: "debug_ping",
		Command:   &cmdAudit,
		StartedAt: timePtr(time.Now()),
	})

	started := time.Now()
	dispatchCtx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	result, err := s.dispatchExec(dispatchCtx, agent.ID, pbReq, requestID, time.Duration(timeoutSec)*time.Second)
	elapsed := time.Since(started).Milliseconds()

	out := pingResponse{
		AgentID:      agent.ID,
		Hostname:     agent.Hostname,
		ElapsedMS:    elapsed,
		DispatchPath: path,
	}
	if err != nil {
		out.Success = false
		out.Error = err.Error()
		jsonResponse(w, http.StatusOK, out)
		return
	}
	resp, ok := result.(*pb.ExecResponse)
	if !ok {
		out.Success = false
		out.Error = "unexpected response type"
		jsonResponse(w, http.StatusOK, out)
		return
	}
	out.Success = resp.Success && resp.Rc == 0
	out.RC = int(resp.Rc)
	if resp.Error != "" {
		out.Error = resp.Error
	}
	jsonResponse(w, http.StatusOK, out)
}

// kindFromExecID derives the operation kind from the request_id prefix.
// dispatchExec callers stamp the ID with a prefix per operation type
// ("exec-", "put-", "fetch-"), so we can recover the kind cheaply
// without threading it through the session struct.
func kindFromExecID(id string) string {
	switch {
	case len(id) >= 4 && id[:4] == "put-":
		return "put_file"
	case len(id) >= 6 && id[:6] == "fetch-":
		return "fetch_file"
	default:
		return "exec"
	}
}
