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
	// OriginObserve verifies and counts but never rejects. Useful for a
	// fleet that wants to watch dirq_agent_origin_violations_total before
	// committing, or to diagnose a fleet where enforcement is dropping
	// something it should not.
	OriginObserve OriginMode = "observe"
	// OriginEnforce drops messages that fail the check. The default: a
	// control that is on by default is the only kind that protects anyone,
	// and the ordering argument says legitimate traffic does not trip it —
	// a relay announces a child (PeerConnected) before forwarding anything
	// from it, and gRPC streams preserve order, so the topology knows where
	// an agent sits before its traffic arrives. What enforcement does drop
	// is a message still in flight down a path the agent has already left;
	// that is stale by definition, and the metric still counts it.
	OriginEnforce OriginMode = "enforce"
)

// DefaultOriginMode is what an unset configuration means.
const DefaultOriginMode = OriginEnforce

// ParseOriginMode maps a config string to a mode. The second return value is
// false when the string was not recognized, so the caller can say so rather
// than letting a typo quietly decide a security setting: an unrecognized value
// resolves to the default, never to the weakest option.
func ParseOriginMode(s string) (OriginMode, bool) {
	switch m := OriginMode(s); m {
	case OriginOff, OriginObserve, OriginEnforce:
		return m, true
	default:
		return DefaultOriginMode, false
	}
}

// originMode returns the configured mode. The zero value is an unset config,
// which means the default rather than the weakest setting — a Server built
// without touching this field enforces.
func (s *Server) originMode() OriginMode {
	if s.cfg.AgentOriginChecks == "" {
		return DefaultOriginMode
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
