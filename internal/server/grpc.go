// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/signutil"
	"github.com/atgreen/dirq/internal/tlsutil"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// isReservedAgentTagKey reports whether a tag key is reserved for operator
// use and must be rejected when supplied by an agent at registration.
// ansible_* tags become ansible_* Ansible inventory host variables; letting
// an agent self-assign them (e.g. ansible_connection=local,
// ansible_python_interpreter=<command>) is a control-node command-execution
// vector. Operators set these per host through the admin tag API instead.
func isReservedAgentTagKey(key string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), "ansible_")
}

// Register handles agent registration. The agent connects once at bootstrap,
// gets assigned an ID, role, and peer list, then disconnects.
func (s *Server) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	regStart := time.Now()
	defer func() { metricRegisterDuration.Observe(time.Since(regStart).Seconds()) }()

	s.log.Info("agent registering", "hostname", req.Hostname, "os", req.Os)

	// Validate registration secret if configured.
	if s.cfg.RegistrationSecret != "" {
		if req.RegistrationSecret != s.cfg.RegistrationSecret {
			s.log.Warn("registration rejected: invalid registration secret", "hostname", req.Hostname)
			metricRegisterTotal.WithLabelValues("rejected_secret").Inc()
			return nil, fmt.Errorf("invalid registration secret")
		}
	}

	// Agents must not be able to self-assign reserved tag keys at
	// registration. ansible_* tags are turned into ansible_* Ansible
	// inventory host variables by the CLI inventory generator
	// (writeInventory) and the collection inventory plugin — including
	// ansible_connection and ansible_python_interpreter. A rogue agent
	// that self-reported them could hijack how the Ansible control node
	// connects to and executes against its host, up to command execution
	// on the controller. Operators can still set these per host via the
	// admin tag API (PUT/PATCH /api/v1/hosts/{id}/tags), which is the
	// trusted channel.
	tags := make(map[string]string)
	for k, v := range req.Tags {
		if isReservedAgentTagKey(k) {
			s.log.Warn("ignoring reserved tag key from agent registration",
				"hostname", req.Hostname, "tag_key", k)
			continue
		}
		tags[k] = v
	}

	// Override the host portion of the agent's ListenAddr with the actual
	// peer IP observed by the server.  The agent resolves its outbound IP
	// via a UDP trick, but that can be wrong in Docker / NAT / multi-homed
	// environments.  The server sees the real routable address on the gRPC
	// connection, so we trust that and keep only the agent's listen port.
	listenAddr := req.ListenAddr
	if p, ok := peer.FromContext(ctx); ok {
		if tcpAddr, ok := p.Addr.(*net.TCPAddr); ok {
			_, port, err := net.SplitHostPort(listenAddr)
			if err == nil {
				listenAddr = net.JoinHostPort(tcpAddr.IP.String(), port)
				if listenAddr != req.ListenAddr {
					s.log.Info("overriding agent listen address with peer IP",
						"hostname", req.Hostname,
						"agent_reported", req.ListenAddr,
						"server_observed", listenAddr,
					)
				}
			}
		}
	}

	agent, err := s.db.RegisterAgent(ctx, db.RegisterAgentParams{
		Hostname:     req.Hostname,
		OS:           req.Os,
		OSVersion:    req.OsVersion,
		Arch:         req.Arch,
		AgentVersion: req.AgentVersion,
		Capabilities: req.Capabilities,
		ListenAddr:   listenAddr,
		Tags:         tags,
		ExecEnabled:  req.ExecEnabled,
	})
	if err != nil {
		metricRegisterTotal.WithLabelValues("rejected_other").Inc()
		return nil, fmt.Errorf("register agent: %w", err)
	}
	metricRegisterTotal.WithLabelValues("ok").Inc()

	// Register the agent's identity with the in-memory topology so role
	// assignment can pick a parent for it.
	s.topology.AddAgent(agent.ID, req.Hostname, listenAddr)

	// Route through the burst-aware batcher so a wave of concurrent
	// registrations gets ZL slots spread across distinct source IPs
	// instead of locking all ZL slots onto whichever VM happened to win
	// the lock-contention race.  Steady-state registrations pay one
	// batch window (~200 ms) of latency, which is invisible compared to
	// the gRPC + TLS handshake cost they already pay.
	sourceIP := ""
	if p, ok := peer.FromContext(ctx); ok {
		if tcpAddr, ok := p.Addr.(*net.TCPAddr); ok {
			sourceIP = tcpAddr.IP.String()
		}
	}
	a := s.regBatch.submit(&pendingReg{
		agentID:    agent.ID,
		hostname:   req.Hostname,
		listenAddr: listenAddr,
		sourceIP:   sourceIP,
		resp:       make(chan assignment, 1),
	})

	roleName := "relay"
	if a.Role == pb.AgentRole_AGENT_ROLE_ZONE_LEADER {
		roleName = "zone_leader"
	}

	s.log.Info("agent registered",
		"hostname", req.Hostname,
		"agent_id", agent.ID,
		"role", roleName,
		"parent_id", a.ParentID,
		"parent_addr", a.ParentAddr,
	)

	// Generate a session token: the server signs the agent ID with its
	// Ed25519 key. Any node with the signing public key (all agents) can
	// verify this token, so relays can authenticate their downstream peers
	// without needing server-side state.
	token := s.signer.SignToken(agent.ID)
	s.sessionMu.Lock()
	s.sessionTokens[agent.ID] = token
	s.sessionMu.Unlock()

	resp := &pb.RegisterResponse{
		AgentId:                    agent.ID,
		Role:                       a.Role,
		ZoneLeaderAddr:             a.ParentAddr,
		HeartbeatIntervalSeconds:   30,
		ServerSigningPublicKey:     s.signer.PublicKey(),
		ServerSigningKeyId:         s.signer.KeyID(),
		ServerSigningPublicKeysOld: s.oldSignerPubKeys,
		FallbackAddrs:              a.FallbackAddrs,
		SessionToken:               token,
	}

	// Issue a per-agent mTLS client certificate if the CA is available.
	if s.caCert != nil && s.caKey != nil {
		certPEM, keyPEM, caCertPEM, err := tlsutil.IssueCert(s.caCert, s.caKey, agent.ID)
		if err != nil {
			s.log.Error("failed to issue mTLS cert", "agent_id", agent.ID, "error", err)
			// Non-fatal: agent can still connect without mTLS (until enforcement is on).
		} else {
			resp.TlsClientCert = certPEM
			resp.TlsClientKey = keyPEM
			resp.TlsCaCert = caCertPEM
			s.log.Info("issued mTLS client cert", "agent_id", agent.ID)
		}
	}

	return resp, nil
}

// RenewCert issues a new mTLS client certificate for an agent whose current
// cert is near expiry. The agent must present its existing (still-valid) cert
// to authenticate. This avoids a full re-registration which would reset topology.
func (s *Server) RenewCert(ctx context.Context, req *pb.RenewCertRequest) (*pb.RenewCertResponse, error) {
	// Verify mTLS CN matches the claimed agent ID.
	cn, ok := TLSCNFromContext(ctx)
	if !ok || cn != req.AgentId {
		s.log.Warn("RenewCert rejected: cert CN mismatch",
			"cert_cn", cn, "claimed_agent_id", req.AgentId)
		return nil, fmt.Errorf("cert CN %q does not match agent_id %q", cn, req.AgentId)
	}

	if s.caCert == nil || s.caKey == nil {
		return nil, fmt.Errorf("CA not configured, cannot issue certificates")
	}

	certPEM, keyPEM, caCertPEM, err := tlsutil.IssueCert(s.caCert, s.caKey, req.AgentId)
	if err != nil {
		s.log.Error("failed to renew mTLS cert", "agent_id", req.AgentId, "error", err)
		return nil, fmt.Errorf("issue cert: %w", err)
	}

	s.log.Info("renewed mTLS client cert", "agent_id", req.AgentId)

	resp := &pb.RenewCertResponse{
		TlsClientCert:              certPEM,
		TlsClientKey:               keyPEM,
		TlsCaCert:                  caCertPEM,
		ServerSigningPublicKey:     s.signer.PublicKey(),
		ServerSigningKeyId:         s.signer.KeyID(),
		ServerSigningPublicKeysOld: s.oldSignerPubKeys,
	}

	return resp, nil
}

// AgentStream handles the persistent bidirectional stream with zone leaders.
func (s *Server) AgentStream(stream pb.DirQServer_AgentStreamServer) error {
	// First message must be a Hello.
	msg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("recv hello: %w", err)
	}

	hello := msg.GetHello()
	if hello == nil {
		return fmt.Errorf("first message must be AgentHello")
	}

	agentID := hello.AgentId

	// If mTLS is active, verify the TLS cert CN matches the claimed agent ID.
	if cn, ok := TLSCNFromContext(stream.Context()); ok && cn != agentID {
		s.log.Warn("agent stream rejected: cert CN mismatch",
			"cert_cn", cn, "claimed_agent_id", agentID)
		return fmt.Errorf("cert CN %q does not match agent_id %q", cn, agentID)
	}

	// Validate session token — reject unauthenticated streams.
	// The token is the server's Ed25519 signature over the agent ID, so we
	// can verify it cryptographically even if the in-memory map was lost
	// (e.g. after a server restart before the agent re-registers).
	if hello.SessionToken == "" {
		s.log.Warn("agent stream rejected: no session token", "agent_id", agentID)
		return fmt.Errorf("agent %s provided no session token", agentID)
	}

	s.sessionMu.RLock()
	expectedToken, hasToken := s.sessionTokens[agentID]
	s.sessionMu.RUnlock()

	tokenValid := false
	if hasToken && hello.SessionToken == expectedToken {
		tokenValid = true
	} else {
		// Verify cryptographically — the token is a signature over the agent ID.
		tokenValid = s.signer.VerifyToken(agentID, hello.SessionToken)
		// If the primary key doesn't accept it, try old keys (key rotation window).
		if !tokenValid && len(s.oldSignerPubKeys) > 0 {
			for _, oldKey := range s.oldSignerPubKeys {
				v, err := signutil.NewVerifier(oldKey, "")
				if err != nil {
					continue
				}
				if v.VerifyToken(agentID, hello.SessionToken) {
					tokenValid = true
					break
				}
			}
		}
	}

	if !tokenValid {
		s.log.Warn("agent stream rejected: invalid session token", "agent_id", agentID)
		return fmt.Errorf("invalid session token for agent %s", agentID)
	}

	s.log.Info("agent stream opened", "agent_id", agentID, "capabilities", hello.Capabilities)

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	as := &agentStream{
		agentID:      agentID,
		capabilities: hello.Capabilities,
		send:         make(chan *pb.ServerMessage, 64),
		cancel:       cancel,
	}

	s.mu.Lock()
	s.streams[agentID] = as
	s.mu.Unlock()
	s.topology.MarkOnline(agentID)

	defer func() {
		s.mu.Lock()
		reassigned := as.reassigned
		delete(s.streams, agentID)
		s.mu.Unlock()
		if reassigned {
			// Agent was demoted/reassigned — it's reconnecting to a
			// new parent, not dead. Don't mark offline.
			s.log.Info("agent stream closed (reassigned)", "agent_id", agentID)
			return
		}

		// Mark the entire subtree offline atomically.  The agent's
		// children (if any) lose their route to the server too because
		// we forwarded broadcasts through this AgentStream, and they
		// can't actually answer until they've reattached to a new
		// parent — which is signaled separately by PeerConnected.
		// Marking only the ZL offline (the pre-fix behavior) left the
		// children as "ghost online" in topology: reachable via the
		// new ZL according to FindZoneLeader, but not actually
		// connected through it yet, so new broadcasts targeted them
		// and timed out.
		subtree := s.topology.MarkSubtreeOffline(agentID)
		if count, err := s.db.MarkAgentTreeOffline(context.Background(), agentID); err != nil {
			s.log.Error("failed to mark agent tree offline", "agent_id", agentID, "error", err)
		} else {
			s.log.Info("agent stream closed, marked subtree offline",
				"agent_id", agentID, "subtree_size", len(subtree), "db_count", count)
		}

		// Notify in-flight broadcast dispatchers so they stop waiting.
		if len(subtree) > 0 {
			s.notifySessionsAgentGone("stream closed", subtree...)
		} else {
			s.notifySessionsAgentGone("stream closed", agentID)
		}

		// Hint direct-stream children where to reconnect.  The
		// topology rewrite is deferred to PeerConnected when the
		// children actually reattach — see reassignOrphans for the
		// rationale.
		go s.reassignOrphans(context.Background(), agentID)
	}()

	// Mark agent online
	if err := s.db.UpdateAgentHeartbeat(ctx, agentID); err != nil {
		s.log.Error("failed to update heartbeat", "error", err)
	}

	// Sender goroutine: pushes messages from the send channel to the stream.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-as.send:
				if err := stream.Send(msg); err != nil {
					s.log.Error("failed to send to agent", "agent_id", agentID, "error", err)
					cancel()
					return
				}
			}
		}
	}()

	// Receiver loop: reads messages from the agent.
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		switch p := msg.Payload.(type) {
		case *pb.AgentMessage_Heartbeat:
			// Legacy heartbeat — ignored. Liveness is now tracked via
			// stream presence and PeerDisconnected notifications.
		case *pb.AgentMessage_PeerConnected:
			// A relay accepted a new child via RelayStream. This is
			// the proof-of-attachment we need to commit the new
			// parent_id and flip the agent back to online — the only
			// case where the server learns about a fallback-parent
			// reattachment without the agent itself re-registering.
			pc := p.PeerConnected
			if pc.AgentId == "" || pc.ParentId == "" {
				break
			}
			s.topology.AssignChild(pc.AgentId, pc.ParentId)
			s.topology.MarkOnline(pc.AgentId)
			if err := s.db.UpdateAgentHeartbeat(ctx, pc.AgentId); err != nil {
				s.log.Error("PeerConnected: heartbeat update failed",
					"agent_id", pc.AgentId, "error", err)
			}
			metricPeerConnectTotal.Inc()
			s.log.Info("peer reattached",
				"agent_id", pc.AgentId, "new_parent", pc.ParentId)
		case *pb.AgentMessage_PeerDisconnected:
			// A relay agent detected a child disconnected. Mark the
			// agent and its entire subtree offline.
			deadID := p.PeerDisconnected.AgentId
			if deadID == "" {
				break
			}
			metricPeerDisconnectTotal.Inc()
			// Snapshot the lost subtree from the in-memory topology
			// BEFORE marking it offline.  PeerDisconnected propagates
			// up the mesh when a relay loses one of its children — the
			// whole subtree below `deadID` is now unreachable through
			// our existing routing.  In-flight broadcasts need to know
			// to stop waiting on those agents.
			affected := s.topology.MarkSubtreeOffline(deadID)
			count, err := s.db.MarkAgentTreeOffline(ctx, deadID)
			if err != nil {
				s.log.Error("failed to mark agent tree offline", "agent_id", deadID, "error", err)
			} else if count > 0 {
				s.log.Info("peer disconnected, marked tree offline",
					"agent_id", deadID, "db_count", count, "mesh_count", len(affected))
			}
			if len(affected) > 0 {
				s.notifySessionsAgentGone("peer disconnected", affected...)
			} else {
				// Topology didn't know about the agent (e.g., never
				// fully registered); still notify by the bare ID.
				s.notifySessionsAgentGone("peer disconnected", deadID)
			}
		case *pb.AgentMessage_QueryResult:
			s.handleQueryResult(p.QueryResult)
		case *pb.AgentMessage_AggregatedResult:
			// Unpack batched results from a zone leader's subtree.
			s.log.Info("received aggregated result",
				"query_id", p.AggregatedResult.QueryId,
				"count", len(p.AggregatedResult.Results),
				"from", agentID,
			)
			for _, r := range p.AggregatedResult.Results {
				s.handleQueryResult(r)
			}
		case *pb.AgentMessage_ExecResponse:
			s.handleExecBroadcastResponse(p.ExecResponse)
		case *pb.AgentMessage_FileChunk:
			s.handleFileChunk(p.FileChunk)
		case *pb.AgentMessage_FetchResponse:
			s.handleFetchResponse(p.FetchResponse)
		case *pb.AgentMessage_DeployResponse:
			s.handleDeployResponse(p.DeployResponse)
		default:
			s.log.Warn("unknown message type from agent", "agent_id", agentID)
		}
	}
}

// RequestPeers handles an agent requesting a new parent after losing connectivity.
func (s *Server) RequestPeers(ctx context.Context, req *pb.PeerRequest) (*pb.PeerResponse, error) {
	// If mTLS is active, verify the TLS cert CN matches the claimed agent ID.
	if cn, ok := TLSCNFromContext(ctx); ok && cn != req.AgentId {
		s.log.Warn("RequestPeers rejected: cert CN mismatch",
			"cert_cn", cn, "claimed_agent_id", req.AgentId)
		return nil, fmt.Errorf("cert CN %q does not match agent_id %q", cn, req.AgentId)
	}

	s.log.Info("agent requesting peers", "agent_id", req.AgentId)

	// RequestPeers does not infer parent liveness. The previous
	// implementation used s.streams[ParentID] as the liveness check, but
	// that map only contains direct server streams (zone leaders) — for
	// any relay parent the lookup is always false and the relay was
	// falsely marked offline whenever any of its leaves rerouted.  Actual
	// parent death is detected via:
	//   - AgentStream close defer  (zone leaders)
	//   - PeerDisconnected upstream (relays)
	//   - The periodic reaper      (server-restart / partition cases)
	// RequestPeers only finds a new parent; it never edits parent state.

	parentID, parentAddr, ok := s.topology.FindShallowestParentWithRoom()
	if !ok || parentID == req.AgentId {
		// No parent has room (tree saturated under churn) — promote the
		// requesting agent to zone leader rather than orphaning it.  This
		// is the escape hatch that keeps the mesh converging when the
		// upper levels temporarily can't absorb a new child.
		s.log.Info("no parent has room, promoting agent to zone_leader",
			"agent_id", req.AgentId)
		s.topology.AssignZoneLeader(req.AgentId)
		return &pb.PeerResponse{
			NewRole: pb.AgentRole_AGENT_ROLE_ZONE_LEADER,
		}, nil
	}

	var fallbacks []string
	for _, fb := range s.topology.FindFallbackParents(parentID, 2) {
		fallbacks = append(fallbacks, fb.ListenAddr)
	}

	s.topology.AssignChild(req.AgentId, parentID)
	parentNode, _ := s.topology.Get(parentID)
	s.log.Info("agent reassigned to new parent",
		"agent_id", req.AgentId, "new_parent", parentNode.Hostname)

	return &pb.PeerResponse{
		ZoneLeaderAddr: parentAddr,
		FallbackAddrs:  fallbacks,
	}, nil
}

// ─────────────────────────────────────────────────────────
// Query dispatch
// ─────────────────────────────────────────────────────────

// querySession tracks an in-flight query.  The embedded
// *sessionAccounting is the single gate every terminal event passes
// through (real QueryResult, synthetic disconnect, fanout failure) so
// the dispatcher counts each agent at most once.
type querySession struct {
	queryID   string
	results   chan *pb.QueryResult
	targetIDs []string
	startedAt time.Time
	timeout   time.Duration
	*sessionAccounting
}

var (
	querySessions   = make(map[string]*querySession)
	querySessionsMu sync.RWMutex
)

// dispatchOutcome reports honest dispatch results so callers can
// distinguish a real completion from a give-up-early idle timeout.
// Today the CLI used to display "Status: completed" regardless, which
// hid bugs where most of the fleet never answered.
type dispatchOutcome struct {
	Results       []*pb.QueryResult
	TotalTargets  int  // size of the original dispatch set
	Responded     int  // agents that returned a result (success or no-match)
	Complete      bool // true iff every target accounted for (no idle/hard timeout)
	HardTimedOut  bool // hard timeout fired before everyone responded
	IdleTimedOut  bool // dispatcher stopped because nothing was arriving
}

// MissingCount returns how many targets never produced a response.
func (o dispatchOutcome) MissingCount() int { return o.TotalTargets - o.Responded }

// dispatchQuery broadcasts a query through all connected zone leaders.
// Zone leaders relay the query down through the mesh to all agents in
// their subtrees. Each agent executes the query locally and results
// bubble back up through the mesh.
func (s *Server) dispatchQuery(ctx context.Context, qr *pb.QueryRequest, targetIDs []string) (dispatchOutcome, error) {
	qs := &querySession{
		queryID:           qr.QueryId,
		results:           make(chan *pb.QueryResult, len(targetIDs)),
		targetIDs:         targetIDs,
		startedAt:         time.Now(),
		timeout:           time.Duration(qr.TimeoutSeconds) * time.Second,
		sessionAccounting: newSessionAccounting(targetIDs),
	}
	if qs.timeout == 0 {
		qs.timeout = 60 * time.Second
	}

	querySessionsMu.Lock()
	querySessions[qr.QueryId] = qs
	querySessionsMu.Unlock()

	// Outcome classification: set by exit path, read by the single defer.
	// Defaults to "complete" because the clean drain at the end is the
	// dominant exit and the hard_timeout / canceled cases override it.
	outcomeLabel := "complete"
	metricInflightSessions.WithLabelValues("query").Inc()
	defer func() {
		metricInflightSessions.WithLabelValues("query").Dec()
		dur := time.Since(qs.startedAt).Seconds()
		missing := len(targetIDs) - qs.AccountedCount()
		if outcomeLabel == "complete" && missing > 0 {
			outcomeLabel = "incomplete"
		}
		metricBroadcastTotal.WithLabelValues("query", outcomeLabel).Inc()
		metricBroadcastDuration.WithLabelValues("query").Observe(dur)
		if missing > 0 {
			metricBroadcastMissingTotal.WithLabelValues("query").Add(float64(missing))
		}
	}()

	defer func() {
		querySessionsMu.Lock()
		delete(querySessions, qr.QueryId)
		querySessionsMu.Unlock()
	}()

	msg := &pb.ServerMessage{
		Payload: &pb.ServerMessage_QueryRequest{
			QueryRequest: qr,
		},
	}
	if err := s.signServerMessage(msg); err != nil {
		return dispatchOutcome{}, fmt.Errorf("sign query request: %w", err)
	}

	// Broadcast to ALL connected zone leaders. Each zone leader executes
	// the query itself AND relays it to its entire subtree.  If a ZL's
	// send buffer is full, its entire subtree won't get the query — we
	// synthesize failures immediately so the dispatcher doesn't wait the
	// hard timeout for responses that can never arrive.
	sent := 0
	var failedSubtrees []string
	s.mu.RLock()
	for _, as := range s.streams {
		select {
		case as.send <- msg:
			sent++
		default:
			s.log.Warn("zone leader send buffer full", "agent_id", as.agentID)
			failedSubtrees = append(failedSubtrees, as.agentID)
		}
	}
	s.mu.RUnlock()

	for _, zlID := range failedSubtrees {
		ids := s.topology.SubtreeIDs(zlID)
		for _, id := range ids {
			s.markGoneInQuerySession(qs, id, "fanout to ZL failed")
		}
	}

	s.log.Info("query dispatched to zone leaders",
		"query_id", qr.QueryId,
		"target_agents", len(targetIDs),
		"zone_leaders", sent,
		"failed_subtrees", len(failedSubtrees),
	)

	// Collect results until every target produces a terminal event
	// (real response OR synthetic disconnect failure injected through
	// handleQueryResult after the first-terminal-wins gate).  The hard
	// timeout is a true backstop — it should rarely fire because
	// stream-loss notifications retire un-reachable agents promptly.
	var results []*pb.QueryResult
	hardTimeout := time.NewTimer(qs.timeout)
	defer hardTimeout.Stop()

	maxResults := len(targetIDs)
	outcome := dispatchOutcome{TotalTargets: maxResults}
	consume := func(r *pb.QueryResult) {
		// Only include successful matches in the returned results.
		// "no match" responses (Success=false, Error="no match") and
		// synthetic disconnect failures count toward completion but
		// are discarded — surfaced via outcome.MissingCount() so the
		// caller can distinguish "no data" from "didn't answer".
		if r.Success {
			results = append(results, r)
		}
	}
	for qs.Remaining() > 0 {
		select {
		case r := <-qs.results:
			consume(r)
		case <-hardTimeout.C:
			s.log.Warn("query hard-timeout fired",
				"query_id", qr.QueryId,
				"accounted", qs.AccountedCount(), "targets", maxResults,
				"still_pending", qs.Remaining())
			outcome.Results = results
			outcome.Responded = qs.AccountedCount()
			outcome.HardTimedOut = true
			outcomeLabel = "hard_timeout"
			return outcome, nil
		case <-ctx.Done():
			outcome.Results = results
			outcome.Responded = qs.AccountedCount()
			outcomeLabel = "canceled"
			return outcome, ctx.Err()
		}
	}

	// Clean-exit drain — ClaimAgent decrements Remaining BEFORE the
	// result is consumed from qs.results, so a burst of concurrent
	// ClaimAgents can race Remaining() to zero while real results still
	// sit in the channel.  Without this drain those late items get GC'd
	// and Responded count under-reports the real response set.
drain:
	for {
		select {
		case r := <-qs.results:
			consume(r)
		default:
			break drain
		}
	}

	outcome.Results = results
	outcome.Responded = qs.AccountedCount()
	outcome.Complete = true
	return outcome, nil
}

func (s *Server) handleQueryResult(result *pb.QueryResult) {
	if result.CollectedAt == nil {
		result.CollectedAt = timestamppb.Now()
	}

	querySessionsMu.RLock()
	qs, ok := querySessions[result.QueryId]
	querySessionsMu.RUnlock()

	if ok {
		// First-terminal-wins.  If a synthetic disconnect failure
		// has already accounted for this agent, drop the late real
		// response — otherwise the dispatcher counts twice and
		// exits early.
		if qs.ClaimAgent(result.AgentId) {
			select {
			case qs.results <- result:
			default:
				s.log.Warn("query result channel full", "query_id", result.QueryId)
			}
		}
	}

	// Stage fact upserts. Coalesces by (agent_id, module) so a burst of
	// updates collapses before reaching the DB. Hot-path: marshal each
	// module's data once here, then last-write-wins in the stage map.
	if result.Success && result.Data != nil {
		s.stageFacts(result.AgentId, result.Data.AsMap())
	}
}

// stageFacts inserts one row per module into the bounded staging map.
// When the map is at its hard cap, NEW keys are dropped (existing keys
// always update for free since coalescing only overwrites). The
// producer never blocks the gRPC receive loop — drops are logged with
// rate limiting.
//
// Critical for scale: JSON marshal happens OUTSIDE the lock.  The
// `packages` module data is tens of KB per agent, and marshaling it
// inside the global factStageMu would serialize the entire AgentStream
// receive path across all 5 zone-leader streams.  At 50k agents with
// ~5ms marshal per response, that's a 250-second floor on broadcast
// fact ingestion if it stayed serialized.  Marshal-then-lock makes the
// expensive work concurrent across goroutines and the lock-hold time
// per response drops to microseconds.
func (s *Server) stageFacts(agentID string, data map[string]any) {
	stageCap := s.cfg.FactStageCap
	if stageCap <= 0 {
		stageCap = defaultFactStageCap
	}
	flushSize := s.cfg.FactFlushSize
	if flushSize <= 0 {
		flushSize = defaultFactFlushSize
	}

	now := time.Now()

	// Marshal every module's data BEFORE taking the lock.  This is the
	// expensive part — keeping it outside lets multiple goroutines do it
	// concurrently instead of serializing on factStageMu.
	type stagedEntry struct {
		key  factKey
		blob []byte
	}
	prepared := make([]stagedEntry, 0, len(data))
	for module, raw := range data {
		md, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		blob, err := json.Marshal(md)
		if err != nil {
			continue
		}
		prepared = append(prepared, stagedEntry{
			key:  factKey{agentID: agentID, module: module},
			blob: blob,
		})
	}

	// Take the lock only to commit prepared entries to the stage map.
	// Lock-hold time is O(len(prepared)) map operations — microseconds.
	dropped := 0
	s.factStageMu.Lock()
	for _, e := range prepared {
		if _, exists := s.factStage[e.key]; !exists && len(s.factStage) >= stageCap {
			dropped++
			continue
		}
		s.factStage[e.key] = db.FactRow{
			AgentID:     e.key.agentID,
			Module:      e.key.module,
			Data:        e.blob,
			CollectedAt: now,
		}
	}
	shouldSignal := len(s.factStage) >= flushSize
	stageLen := len(s.factStage)
	s.factStageMu.Unlock()

	if dropped > 0 {
		// Rate-limit to one warning per second so a sustained burst
		// doesn't drown the log.
		if time.Since(s.factDropLogged) >= time.Second {
			s.factDropLogged = now
			s.log.Warn("fact stage full, dropped new keys", "dropped", dropped, "stage_len", stageLen, "cap", stageCap)
		}
	}
	if shouldSignal {
		select {
		case s.factFlushSignal <- struct{}{}:
		default:
		}
	}
}

const (
	defaultFactFlushInterval = 250 * time.Millisecond
	defaultFactFlushSize     = 5000
	defaultFactStageCap      = 20000
	// Postgres can swallow any reasonable batch in one unnest() call;
	// SQLite caps itself at sqliteFactBulkChunk in the bulk path. We
	// chunk here to bound per-statement work and memory peaks.
	factWriteChunk = 5000
)

// runFactBatcher flushes the staging map periodically and on size
// threshold. Snapshots the map under the lock, releases the lock, then
// performs the bulk write — so a slow DB can't backpressure ingest.
func (s *Server) runFactBatcher(ctx context.Context) {
	interval := s.cfg.FactFlushInterval
	if interval <= 0 {
		interval = defaultFactFlushInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.flushFactStage(context.Background())
			return
		case <-ticker.C:
			s.flushFactStage(ctx)
		case <-s.factFlushSignal:
			s.flushFactStage(ctx)
		}
	}
}

// flushFactStage atomically swaps the staging map for a fresh one and
// bulk-writes the snapshot in chunks. Errors are logged and dropped —
// the next query result for the same key will re-stage and retry.
func (s *Server) flushFactStage(ctx context.Context) {
	s.factStageMu.Lock()
	if len(s.factStage) == 0 {
		s.factStageMu.Unlock()
		return
	}
	snapshot := s.factStage
	s.factStage = make(map[factKey]db.FactRow, len(snapshot))
	s.factStageMu.Unlock()

	rows := make([]db.FactRow, 0, len(snapshot))
	for _, r := range snapshot {
		rows = append(rows, r)
	}

	writeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	backend := s.db.Kind()
	flushStart := time.Now()
	flushOK := true
	for start := 0; start < len(rows); start += factWriteChunk {
		end := start + factWriteChunk
		if end > len(rows) {
			end = len(rows)
		}
		if err := s.db.BulkUpsertFacts(writeCtx, rows[start:end]); err != nil {
			s.log.Error("bulk fact upsert failed", "error", err, "rows", end-start)
			flushOK = false
		}
	}
	if flushOK {
		metricFactFlushTotal.WithLabelValues(backend, "ok").Inc()
	} else {
		metricFactFlushTotal.WithLabelValues(backend, "error").Inc()
	}
	metricFactFlushDuration.WithLabelValues(backend).Observe(time.Since(flushStart).Seconds())
}

