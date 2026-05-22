// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/signutil"
	"github.com/atgreen/dirq/internal/tlsutil"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// Register handles agent registration. The agent connects once at bootstrap,
// gets assigned an ID, role, and peer list, then disconnects.
func (s *Server) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	s.log.Info("agent registering", "hostname", req.Hostname, "os", req.Os)

	// Validate registration secret if configured.
	if s.cfg.RegistrationSecret != "" {
		if req.RegistrationSecret != s.cfg.RegistrationSecret {
			s.log.Warn("registration rejected: invalid registration secret", "hostname", req.Hostname)
			return nil, fmt.Errorf("invalid registration secret")
		}
	}

	tags := make(map[string]string)
	for k, v := range req.Tags {
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
		return nil, fmt.Errorf("register agent: %w", err)
	}

	// Register the agent's identity with the in-memory topology so role
	// assignment can pick a parent for it.  All decisions happen against
	// in-memory maps — no DB round-trips, no global lock, microseconds.
	s.topology.AddAgent(agent.ID, req.Hostname, listenAddr)
	a := s.assignRole(agent.ID)

	roleName := "relay"
	switch a.Role {
	case pb.AgentRole_AGENT_ROLE_ZONE_LEADER:
		roleName = "zone_leader"
		s.topology.AssignZoneLeader(agent.ID)
	default:
		if !s.topology.AssignChild(agent.ID, a.ParentID) {
			// Race: chosen parent filled up between FindShallowest and
			// AssignChild.  Fall back to promoting this agent to ZL so
			// it gets a route instead of being orphaned.
			s.log.Warn("topology: chosen parent full at commit, promoting to zone_leader",
				"agent_id", agent.ID, "intended_parent", a.ParentID)
			s.topology.AssignZoneLeader(agent.ID)
			a = assignment{Role: pb.AgentRole_AGENT_ROLE_ZONE_LEADER}
			roleName = "zone_leader"
		}
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
		s.topology.MarkOffline(agentID)
		if err := s.db.SetAgentOffline(context.Background(), agentID); err != nil {
			s.log.Error("failed to mark agent offline", "agent_id", agentID, "error", err)
		}
		s.log.Info("agent stream closed, marked offline", "agent_id", agentID)

		// If this was a zone leader, reassign its orphaned children to
		// healthy parents so they don't stay stuck under a dead node.
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
		case *pb.AgentMessage_PeerDisconnected:
			// A relay agent detected a child disconnected. Mark the
			// agent and its entire subtree offline — unless the agent
			// is being reassigned by the rebalancer (expected disconnect).
			deadID := p.PeerDisconnected.AgentId
			if deadID == "" {
				break
			}
			s.reassigningMu.Lock()
			_, isReassigning := s.reassigning[deadID]
			if isReassigning {
				delete(s.reassigning, deadID)
			}
			s.reassigningMu.Unlock()
			if isReassigning {
				s.log.Info("peer disconnected (reassigning, skipped)", "agent_id", deadID)
				break
			}
			count, err := s.db.MarkAgentTreeOffline(ctx, deadID)
			if err != nil {
				s.log.Error("failed to mark agent tree offline", "agent_id", deadID, "error", err)
			} else if count > 0 {
				s.log.Info("peer disconnected, marked tree offline", "agent_id", deadID, "count", count)
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

	// The agent's current parent may be unreachable. Only mark it offline
	// if the server doesn't have an active stream for it (i.e., it's
	// genuinely dead, not just slow). This avoids accidentally killing
	// healthy nodes that the agent was just reassigned to.
	if n, ok := s.topology.Get(req.AgentId); ok && n.ParentID != "" {
		s.mu.RLock()
		_, parentAlive := s.streams[n.ParentID]
		s.mu.RUnlock()
		if !parentAlive {
			s.topology.MarkOffline(n.ParentID)
			s.db.SetAgentOffline(ctx, n.ParentID)
			s.log.Info("marked failed parent offline", "parent_id", n.ParentID)
		}
	}

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

// querySession tracks an in-flight query.
type querySession struct {
	queryID   string
	results   chan *pb.QueryResult
	targetIDs []string
	startedAt time.Time
	timeout   time.Duration

	// Tracking for /api/v1/debug/inflight. receivedAgents is the set of
	// agent IDs that have answered (success or no-match); set-diff with
	// targetIDs is the still-missing set.
	receivedMu     sync.Mutex
	receivedAgents map[string]bool
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
		queryID:        qr.QueryId,
		results:        make(chan *pb.QueryResult, len(targetIDs)),
		targetIDs:      targetIDs,
		startedAt:      time.Now(),
		timeout:        time.Duration(qr.TimeoutSeconds) * time.Second,
		receivedAgents: make(map[string]bool, len(targetIDs)),
	}
	if qs.timeout == 0 {
		qs.timeout = 60 * time.Second
	}

	querySessionsMu.Lock()
	querySessions[qr.QueryId] = qs
	querySessionsMu.Unlock()

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
	// the query itself AND relays it to its entire subtree. Results from
	// leaf and relay agents bubble back up through the mesh.
	sent := 0
	s.mu.RLock()
	for _, as := range s.streams {
		select {
		case as.send <- msg:
			sent++
		default:
			s.log.Warn("zone leader send buffer full", "agent_id", as.agentID)
		}
	}
	s.mu.RUnlock()

	s.log.Info("query dispatched to zone leaders",
		"query_id", qr.QueryId,
		"target_agents", len(targetIDs),
		"zone_leaders", sent,
	)

	// Collect results until all targets respond or the hard timeout expires.
	// Agents send "no match" responses when they don't match the WHERE clause,
	// so the server can count completions and stop as soon as all targets
	// have answered. The idle timeout is a safety net for agents that crash
	// or are unreachable through the mesh — but it has to scale with the
	// fleet, since a wider fan-out spreads responses over a longer interval
	// even on a healthy mesh.  Floor of 2s for tiny fleets, +1s per 500
	// targets thereafter, hard cap of 30s.
	idleWindow := 2 * time.Second
	if extra := time.Duration(len(targetIDs)/500) * time.Second; extra > 0 {
		idleWindow += extra
	}
	if idleWindow > 30*time.Second {
		idleWindow = 30 * time.Second
	}

	var results []*pb.QueryResult
	hardTimeout := time.NewTimer(qs.timeout)
	defer hardTimeout.Stop()
	idleTimeout := time.NewTimer(idleWindow)
	defer idleTimeout.Stop()

	responded := 0
	maxResults := len(targetIDs)
	outcome := dispatchOutcome{TotalTargets: maxResults}
	for responded < maxResults {
		select {
		case r := <-qs.results:
			responded++
			// Record which agent answered (match or no-match) so the
			// debug/inflight endpoint can compute the still-missing set.
			qs.receivedMu.Lock()
			qs.receivedAgents[r.AgentId] = true
			qs.receivedMu.Unlock()
			// Only include successful matches in the returned results.
			// "no match" responses count toward completion but are discarded.
			if r.Success {
				results = append(results, r)
			}
			if !idleTimeout.Stop() {
				select {
				case <-idleTimeout.C:
				default:
				}
			}
			idleTimeout.Reset(idleWindow)
		case <-idleTimeout.C:
			s.log.Info("query stopped on idle timeout",
				"query_id", qr.QueryId, "received", responded, "targets", maxResults,
				"idle_window", idleWindow)
			outcome.Results = results
			outcome.Responded = responded
			outcome.IdleTimedOut = true
			return outcome, nil
		case <-hardTimeout.C:
			s.log.Warn("query hard-timeout fired",
				"query_id", qr.QueryId, "received", responded, "targets", maxResults)
			outcome.Results = results
			outcome.Responded = responded
			outcome.HardTimedOut = true
			return outcome, nil
		case <-ctx.Done():
			outcome.Results = results
			outcome.Responded = responded
			return outcome, ctx.Err()
		}
	}

	outcome.Results = results
	outcome.Responded = responded
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
		select {
		case qs.results <- result:
		default:
			s.log.Warn("query result channel full", "query_id", result.QueryId)
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
	s.factStageMu.Lock()
	dropped := 0
	for module, raw := range data {
		md, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		key := factKey{agentID: agentID, module: module}
		if _, exists := s.factStage[key]; !exists && len(s.factStage) >= stageCap {
			dropped++
			continue
		}
		blob, err := json.Marshal(md)
		if err != nil {
			continue
		}
		s.factStage[key] = db.FactRow{
			AgentID:     agentID,
			Module:      module,
			Data:        blob,
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

	for start := 0; start < len(rows); start += factWriteChunk {
		end := start + factWriteChunk
		if end > len(rows) {
			end = len(rows)
		}
		if err := s.db.BulkUpsertFacts(writeCtx, rows[start:end]); err != nil {
			s.log.Error("bulk fact upsert failed", "error", err, "rows", end-start)
		}
	}
}

