// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// notifySessionsAgentGone tells every in-flight broadcast dispatcher
// that the listed agents won't respond — they crashed, their stream
// closed, the reaper expired them, or their mesh path went away.
// Each dispatcher's accounting goes through ClaimAgent so a stream-loss
// notification and a late real response never both count.
//
// Implementation notes (per the codex review of the design):
//
//   - Snapshot session pointers under each global session mutex, then
//     release before touching individual sessions.  Writing to a
//     result channel while holding the global lock would block stream
//     close handling, which would delay reassignOrphans.
//
//   - Synthetic failures are injected through each session's results
//     channel using the same first-terminal-wins gate that real
//     responses pass through.  The receive loop is uniform — every
//     terminal event looks identical regardless of source.
//
//   - Best-effort non-blocking enqueue.  If a session's results channel
//     is full, the agent stays in its pending set; the dispatcher will
//     drain the channel and converge on the next iteration.  The hard
//     timeout is the ultimate backstop.
func (s *Server) notifySessionsAgentGone(reason string, agentIDs ...string) {
	if len(agentIDs) == 0 {
		return
	}

	// Snapshot session pointers under each global mutex, release, then
	// notify each session inline.
	querySessionsMu.RLock()
	qSessions := make([]*querySession, 0, len(querySessions))
	for _, qs := range querySessions {
		qSessions = append(qSessions, qs)
	}
	querySessionsMu.RUnlock()

	execBroadcastSessionsMu.RLock()
	eSessions := make([]*execBroadcastSession, 0, len(execBroadcastSessions))
	for _, bs := range execBroadcastSessions {
		eSessions = append(eSessions, bs)
	}
	execBroadcastSessionsMu.RUnlock()

	deploySessionsMu.RLock()
	dSessions := make([]*deploySession, 0, len(deploySessions))
	for _, ds := range deploySessions {
		dSessions = append(dSessions, ds)
	}
	deploySessionsMu.RUnlock()

	for _, agentID := range agentIDs {
		for _, qs := range qSessions {
			s.markGoneInQuerySession(qs, agentID, reason)
		}
		for _, bs := range eSessions {
			s.markGoneInExecSession(bs, agentID, reason)
		}
		for _, ds := range dSessions {
			s.markGoneInDeploySession(ds, agentID, reason)
		}
	}
}

func (s *Server) markGoneInQuerySession(qs *querySession, agentID, reason string) {
	if !qs.ClaimAgent(agentID) {
		return
	}
	// Synthesize a failure that counts toward completion but is omitted
	// from returned results (dispatcher discards Success=false QueryResults).
	hostname := ""
	if n, ok := s.topology.Get(agentID); ok {
		hostname = n.Hostname
	}
	synth := &pb.QueryResult{
		QueryId:  qs.queryID,
		AgentId:  agentID,
		Hostname: hostname,
		Success:  false,
		Error:    "agent went away: " + reason,
	}
	select {
	case qs.results <- synth:
	default:
		// Channel full — accounting already updated, dispatcher will
		// see Remaining()==0 on its next drain even if this synth
		// itself is dropped.
	}
}

func (s *Server) markGoneInExecSession(bs *execBroadcastSession, agentID, reason string) {
	if !bs.ClaimAgent(agentID) {
		return
	}
	hostname := ""
	if n, ok := s.topology.Get(agentID); ok {
		hostname = n.Hostname
	}
	synth := &pb.ExecResponse{
		RequestId: bs.requestID,
		AgentId:   agentID,
		Hostname:  hostname,
		Success:   false,
		Error:     "agent went away: " + reason,
		Rc:        -1,
	}
	select {
	case bs.results <- synth:
	default:
	}
}

func (s *Server) markGoneInDeploySession(ds *deploySession, agentID, reason string) {
	if !ds.ClaimAgent(agentID) {
		return
	}
	hostname := ""
	if n, ok := s.topology.Get(agentID); ok {
		hostname = n.Hostname
	}
	synth := &pb.DeployResponse{
		RequestId: ds.requestID,
		AgentId:   agentID,
		Hostname:  hostname,
		Success:   false,
		Error:     "agent went away: " + reason,
		Phase:     "transport",
		Rc:        -1,
	}
	select {
	case ds.results <- synth:
	default:
	}
}
