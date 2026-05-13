// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/atgreen/dirq/internal/db"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// Register handles agent registration. The agent connects once at bootstrap,
// gets assigned an ID, role, and peer list, then disconnects.
func (s *Server) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	s.log.Info("agent registering", "hostname", req.Hostname, "os", req.Os)

	tags := make(map[string]string)
	for k, v := range req.Tags {
		tags[k] = v
	}

	agent, err := s.db.RegisterAgent(ctx, db.RegisterAgentParams{
		Hostname:     req.Hostname,
		OS:           req.Os,
		OSVersion:    req.OsVersion,
		Arch:         req.Arch,
		AgentVersion: req.AgentVersion,
		Capabilities: req.Capabilities,
		ListenAddr:   req.ListenAddr,
		Tags:         tags,
		ExecEnabled:  req.ExecEnabled,
	})
	if err != nil {
		return nil, fmt.Errorf("register agent: %w", err)
	}

	// Assign role under an advisory lock to prevent races when thousands
	// of agents register concurrently. Without this, multiple agents can
	// all see "4 zone leaders" and all become the 5th, or two agents can
	// be assigned to a parent that only has 1 slot left.
	var a assignment
	err = s.db.WithTopologyLock(ctx, func() error {
		var assignErr error
		a, assignErr = s.assignRole(ctx)
		if assignErr != nil {
			return assignErr
		}

		roleName := "relay"
		switch a.Role {
		case pb.AgentRole_AGENT_ROLE_ZONE_LEADER:
			roleName = "zone_leader"
		}
		if err := s.db.SetAgentRole(ctx, agent.ID, roleName); err != nil {
			return err
		}
		if a.ParentID != "" {
			if err := s.db.SetAgentParent(ctx, agent.ID, a.ParentID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.log.Error("topology assignment failed, defaulting to zone_leader", "error", err)
		a = assignment{Role: pb.AgentRole_AGENT_ROLE_ZONE_LEADER}
		s.db.SetAgentRole(ctx, agent.ID, "zone_leader")
	}

	roleName := "relay"
	switch a.Role {
	case pb.AgentRole_AGENT_ROLE_ZONE_LEADER:
		roleName = "zone_leader"
	}

	s.log.Info("agent registered",
		"hostname", req.Hostname,
		"agent_id", agent.ID,
		"role", roleName,
		"parent_id", a.ParentID,
		"parent_addr", a.ParentAddr,
	)

	return &pb.RegisterResponse{
		AgentId:                  agent.ID,
		Role:                     a.Role,
		ZoneLeaderAddr:           a.ParentAddr,
		HeartbeatIntervalSeconds: 30,
		ServerSigningPublicKey:   s.signer.PublicKey(),
		ServerSigningKeyId:       s.signer.KeyID(),
		FallbackAddrs:            a.FallbackAddrs,
	}, nil
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
		if err := s.db.SetAgentOffline(context.Background(), agentID); err != nil {
			s.log.Error("failed to mark agent offline", "agent_id", agentID, "error", err)
		}
		s.log.Info("agent stream closed, marked offline", "agent_id", agentID)
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
			s.handleExecResponse(p.ExecResponse)
		case *pb.AgentMessage_FileChunk:
			s.handleFileChunk(p.FileChunk)
		case *pb.AgentMessage_FetchResponse:
			s.handleFetchResponse(p.FetchResponse)
		default:
			s.log.Warn("unknown message type from agent", "agent_id", agentID)
		}
	}
}

// RequestPeers handles an agent requesting new peers after losing connectivity.
func (s *Server) RequestPeers(ctx context.Context, req *pb.PeerRequest) (*pb.PeerResponse, error) {
	s.log.Info("agent requesting peers", "agent_id", req.AgentId)
	// For now, tell it to connect as a zone leader.
	return &pb.PeerResponse{
		Peers:          nil,
		ZoneLeaderAddr: "",
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
}

var (
	querySessions   = make(map[string]*querySession)
	querySessionsMu sync.RWMutex
)

// dispatchQuery broadcasts a query through all connected zone leaders.
// Zone leaders relay the query down through the mesh to all agents in
// their subtrees. Each agent executes the query locally and results
// bubble back up through the mesh.
func (s *Server) dispatchQuery(ctx context.Context, qr *pb.QueryRequest, targetIDs []string) ([]*pb.QueryResult, error) {
	qs := &querySession{
		queryID:   qr.QueryId,
		results:   make(chan *pb.QueryResult, len(targetIDs)),
		targetIDs: targetIDs,
		startedAt: time.Now(),
		timeout:   time.Duration(qr.TimeoutSeconds) * time.Second,
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
		return nil, fmt.Errorf("sign query request: %w", err)
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

	// Collect results until the hard timeout expires or no new results arrive
	// for 3 seconds (idle timeout). Agents that don't match the WHERE clause
	// won't respond at all, so we can't wait for a fixed count.
	var results []*pb.QueryResult
	hardTimeout := time.NewTimer(qs.timeout)
	defer hardTimeout.Stop()
	idleTimeout := time.NewTimer(5 * time.Second)
	defer idleTimeout.Stop()

	maxResults := len(targetIDs)
	for len(results) < maxResults {
		select {
		case r := <-qs.results:
			results = append(results, r)
			// Reset idle timer — more results may be coming.
			if !idleTimeout.Stop() {
				select {
				case <-idleTimeout.C:
				default:
				}
			}
			idleTimeout.Reset(5 * time.Second)
		case <-idleTimeout.C:
			// No results for 3s — assume all matching agents have responded.
			return results, nil
		case <-hardTimeout.C:
			s.log.Warn("query timed out", "query_id", qr.QueryId, "received", len(results), "targets", maxResults)
			return results, nil
		case <-ctx.Done():
			return results, ctx.Err()
		}
	}

	return results, nil
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

	// Also cache the facts in the database.
	if result.Success && result.Data != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			data := result.Data.AsMap()
			for module, moduleData := range data {
				if md, ok := moduleData.(map[string]any); ok {
					if err := s.db.UpsertFact(ctx, result.AgentId, module, md); err != nil {
						s.log.Error("failed to cache fact", "agent_id", result.AgentId, "module", module, "error", err)
					}
				}
			}
		}()
	}
}
