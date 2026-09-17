// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import "log/slog"

// Origin checks: does the identity a message claims match the stream it
// arrived on?
//
// A zone leader's AgentStream carries its entire subtree multiplexed onto one
// connection, so the mTLS CN authenticates the zone leader and nothing else.
// Every message names its own origin in a payload field (QueryResult.AgentId,
// ExecResponse.AgentId, PeerDisconnected.AgentId, …), and those fields used to
// be believed without question — so any agent could answer for any host,
// overwrite any host's facts, or mark any host offline (dirq-632.2).
//
// This is Layer 1 of the fix: no cryptography, just the two facts the server
// already has — who the stream authenticated as, and what the topology says
// sits beneath them. It bounds a compromised *leaf* to speaking only for
// itself. A compromised relay or zone leader can still speak for its own
// subtree; closing that needs per-payload attestation (Layer 2).

// OriginMode selects what happens when a claim fails its origin check.
type OriginMode string

const (
	// OriginOff disables the topology-derived checks entirely. The target
	// check in the exec/file handlers still applies — it is not
	// topology-derived and so carries no false-positive risk.
	OriginOff OriginMode = "off"
	// OriginObserve verifies and counts but never rejects. This is the
	// default: reattachment lag can make a legitimate late message look
	// foreign, and the metric is how we find out how often that happens
	// before anyone turns on enforcement.
	OriginObserve OriginMode = "observe"
	// OriginEnforce drops messages that fail the check.
	OriginEnforce OriginMode = "enforce"
)

// ParseOriginMode maps a config string to a mode, falling back to observe for
// anything unrecognized — an operator typo must not silently disable the
// check, and must not silently start dropping traffic either.
func ParseOriginMode(s string) OriginMode {
	switch OriginMode(s) {
	case OriginOff:
		return OriginOff
	case OriginEnforce:
		return OriginEnforce
	default:
		return OriginObserve
	}
}

// originMode returns the configured mode, defaulting an unset config to
// observe.
func (s *Server) originMode() OriginMode {
	if s.cfg.AgentOriginChecks == "" {
		return OriginObserve
	}
	return s.cfg.AgentOriginChecks
}

// originAllows reports whether a message claiming to originate at claimedID
// may be acted on when it arrived over the stream authenticated as originID.
//
// A claim is legitimate when the claimant is the sender itself, or sits
// beneath the sender in the mesh — which is exactly the set of agents whose
// traffic that sender is supposed to be relaying. Anything else is a sender
// speaking for a part of the fleet it has no route to.
//
// kind labels the message for the metric and the log; it is a small closed set
// ("exec_response", "query_result", …), never operator- or agent-supplied text.
func (s *Server) originAllows(originID, claimedID, kind string) bool {
	mode := s.originMode()
	if mode == OriginOff {
		return true
	}
	if originID != "" && (claimedID == originID || s.topology.IsWithinSubtree(originID, claimedID)) {
		return true
	}

	action := "observed"
	if mode == OriginEnforce {
		action = "rejected"
	}
	metricOriginViolations.WithLabelValues(kind, action).Inc()
	s.log.Warn("agent message claims an origin outside the sending stream's subtree",
		slog.String("kind", kind),
		slog.String("stream_agent_id", originID),
		slog.String("claimed_agent_id", claimedID),
		slog.String("action", action),
	)
	return mode != OriginEnforce
}

// originTargetMismatch reports whether a terminal response names an agent
// other than the one its session was dispatched to.
//
// Unlike originAllows this consults no topology, so there is no lag to be
// wrong about and it applies in every mode: a session dispatched to host A is
// never satisfied by a response from host B, whoever relayed it.
func (s *Server) originTargetMismatch(sessionTarget, claimedID, kind string) bool {
	if sessionTarget == "" || claimedID == sessionTarget {
		return false
	}
	metricOriginViolations.WithLabelValues(kind, "rejected").Inc()
	s.log.Warn("response names an agent other than the request's target",
		slog.String("kind", kind),
		slog.String("session_target", sessionTarget),
		slog.String("claimed_agent_id", claimedID),
	)
	return true
}
