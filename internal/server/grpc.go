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
	})
	if err != nil {
		return nil, fmt.Errorf("register agent: %w", err)
	}

	// For now, assign all agents as zone leaders (direct connection).
	// Topology manager will reassign roles as the fleet grows.
	role := pb.AgentRole_AGENT_ROLE_ZONE_LEADER
	if err := s.db.SetAgentRole(ctx, agent.ID, "zone_leader"); err != nil {
		s.log.Error("failed to set agent role", "error", err)
	}

	return &pb.RegisterResponse{
		AgentId:                  agent.ID,
		Role:                    role,
		Peers:                   nil, // no peers for zone leaders
		ZoneLeaderAddr:          "",  // this IS the zone leader
		HeartbeatIntervalSeconds: 30,
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
		delete(s.streams, agentID)
		s.mu.Unlock()
		// Mark agent offline immediately when stream drops.
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
			if err := s.db.UpdateAgentHeartbeat(ctx, agentID); err != nil {
				s.log.Error("heartbeat update failed", "error", err)
			}
		case *pb.AgentMessage_QueryResult:
			s.handleQueryResult(p.QueryResult)
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
	queryID    string
	results    chan *pb.QueryResult
	targetIDs  []string
	startedAt  time.Time
	timeout    time.Duration
}

var (
	querySessions   = make(map[string]*querySession)
	querySessionsMu sync.RWMutex
)

// dispatchQuery sends a query to the target agents and collects results.
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

	// Fan out query to target agents.
	msg := &pb.ServerMessage{
		Payload: &pb.ServerMessage_QueryRequest{
			QueryRequest: qr,
		},
	}

	sent := 0
	s.mu.RLock()
	for _, id := range targetIDs {
		if as, ok := s.streams[id]; ok {
			select {
			case as.send <- msg:
				sent++
			default:
				s.log.Warn("agent send buffer full", "agent_id", id)
			}
		}
	}
	s.mu.RUnlock()

	s.log.Info("query dispatched", "query_id", qr.QueryId, "targets", len(targetIDs), "sent", sent)

	// Collect results until timeout or all responded.
	var results []*pb.QueryResult
	timer := time.NewTimer(qs.timeout)
	defer timer.Stop()

	for len(results) < sent {
		select {
		case r := <-qs.results:
			results = append(results, r)
		case <-timer.C:
			s.log.Warn("query timed out", "query_id", qr.QueryId, "received", len(results), "expected", sent)
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
