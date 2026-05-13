// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/atgreen/dirq/internal/modules"
	"github.com/atgreen/dirq/internal/query"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// Config holds agent configuration.
type Config struct {
	ServerAddr  string            // DirQ server address for bootstrap
	ListenAddr  string            // address this agent listens on for downstream peers
	Tags        map[string]string // user-defined tags
	Version     string
	ExecEnabled bool // Phase 2: whether this agent accepts exec/file requests
}

// Agent is the DirQ endpoint agent.
type Agent struct {
	pb.UnimplementedDirQRelayServer

	cfg     Config
	log     *slog.Logger
	agentID string
	role       pb.AgentRole
	parentAddr string // where to connect upstream (server addr or parent's listen_addr)

	// gRPC server for downstream peers
	grpcSv *grpc.Server

	// Upstream connection (to server or parent peer)
	upstreamConn   *grpc.ClientConn
	upstreamStream pb.DirQServer_AgentStreamClient

	// Connected downstream peers
	mu          sync.RWMutex
	downstreams map[string]*downstreamPeer
}

type downstreamPeer struct {
	agentID string
	send    chan *pb.ServerMessage
	cancel  context.CancelFunc
}

// New creates a new agent.
func New(cfg Config, log *slog.Logger) *Agent {
	return &Agent{
		cfg:         cfg,
		log:         log,
		downstreams: make(map[string]*downstreamPeer),
	}
}

// Run starts the agent: registers with server, opens stream, listens for peers.
// If the upstream connection drops, Run reconnects with exponential backoff.
func (a *Agent) Run(ctx context.Context) error {
	// Step 1: Register with the server (retry until success or context cancelled).
	if err := a.registerWithRetry(ctx); err != nil {
		return err
	}
	a.log.Info("registered", "agent_id", a.agentID, "role", a.role)

	// Step 2: Start listening for downstream peers.
	if a.cfg.ListenAddr != "" {
		go a.startRelayServer(ctx)
	}

	// Step 3: Connect and run, reconnecting on failure.
	return a.connectLoop(ctx)
}

// registerWithRetry attempts registration with exponential backoff.
func (a *Agent) registerWithRetry(ctx context.Context) error {
	backoff := 1 * time.Second
	maxBackoff := 60 * time.Second

	for {
		err := a.register(ctx)
		if err == nil {
			return nil
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		a.log.Warn("registration failed, retrying", "error", err, "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = backoff * 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// connectLoop connects upstream and runs the main loop. If the connection
// drops, it reconnects with exponential backoff. Runs until ctx is cancelled.
func (a *Agent) connectLoop(ctx context.Context) error {
	backoff := 1 * time.Second
	maxBackoff := 60 * time.Second

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := a.connectUpstream(ctx)
		if err != nil {
			a.log.Warn("upstream connection failed, retrying", "error", err, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff = backoff * 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		// Reset backoff on successful connect.
		backoff = 1 * time.Second

		a.log.Info("upstream connected", "server", a.cfg.ServerAddr)

		// Run the main loop until the stream breaks.
		err = a.mainLoop(ctx)

		// Clean up the old connection.
		if a.upstreamConn != nil {
			a.upstreamConn.Close()
			a.upstreamConn = nil
		}
		a.upstreamStream = nil

		if ctx.Err() != nil {
			return ctx.Err()
		}

		a.log.Warn("upstream connection lost, reconnecting", "error", err, "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = backoff * 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (a *Agent) register(ctx context.Context) error {
	conn, err := grpc.NewClient(a.cfg.ServerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("connect to server: %w", err)
	}
	defer conn.Close()

	client := pb.NewDirQServerClient(conn)

	hostname, _ := os.Hostname()

	caps := []string{}
	for name := range modules.Registry() {
		caps = append(caps, name)
	}

	resp, err := client.Register(ctx, &pb.RegisterRequest{
		Hostname:     hostname,
		Os:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		AgentVersion: a.cfg.Version,
		Capabilities: caps,
		ListenAddr:   a.cfg.ListenAddr,
		Tags:         a.cfg.Tags,
		ExecEnabled:  a.cfg.ExecEnabled,
	})
	if err != nil {
		return fmt.Errorf("register RPC: %w", err)
	}

	a.agentID = resp.AgentId
	a.role = resp.Role

	// Determine where to connect upstream.
	if resp.ZoneLeaderAddr != "" && resp.Role != pb.AgentRole_AGENT_ROLE_ZONE_LEADER {
		// Connect to assigned parent (zone leader or relay peer).
		a.parentAddr = resp.ZoneLeaderAddr
		a.log.Info("assigned to parent", "parent_addr", a.parentAddr, "role", resp.Role)
	} else {
		// Zone leader — connect directly to the server.
		a.parentAddr = a.cfg.ServerAddr
	}

	return nil
}

func (a *Agent) connectUpstream(ctx context.Context) error {
	target := a.parentAddr
	if target == "" {
		target = a.cfg.ServerAddr
	}

	a.log.Info("connecting upstream", "target", target, "role", a.role)

	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                20 * time.Second, // ping server every 20s if idle
			Timeout:             10 * time.Second, // wait 10s for pong
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return fmt.Errorf("connect upstream: %w", err)
	}
	a.upstreamConn = conn

	// Zone leaders connect to the server's AgentStream RPC.
	// Relays and leafs connect to their parent's RelayStream RPC.
	if a.role == pb.AgentRole_AGENT_ROLE_ZONE_LEADER {
		client := pb.NewDirQServerClient(conn)
		stream, err := client.AgentStream(ctx)
		if err != nil {
			return fmt.Errorf("open agent stream: %w", err)
		}
		a.upstreamStream = stream
	} else {
		client := pb.NewDirQRelayClient(conn)
		stream, err := client.RelayStream(ctx)
		if err != nil {
			// Fall back to direct server connection if parent is unreachable.
			a.log.Warn("parent unreachable, falling back to server",
				"parent", target, "error", err)
			conn.Close()
			return a.connectToServer(ctx)
		}
		a.upstreamStream = stream
	}

	return a.sendHello()
}

// sendHello sends an AgentHello on the upstream stream. Must be the first
// message on any new stream — the server/relay rejects streams without it.
func (a *Agent) sendHello() error {
	caps := []string{}
	for name := range modules.Registry() {
		caps = append(caps, name)
	}
	return a.upstreamStream.Send(&pb.AgentMessage{
		Payload: &pb.AgentMessage_Hello{
			Hello: &pb.AgentHello{
				AgentId:      a.agentID,
				Capabilities: caps,
				ExecEnabled:  a.cfg.ExecEnabled,
			},
		},
	})
}

// connectToServer is the fallback when a parent peer is unreachable.
// It connects directly to the DirQ server as if this were a zone leader.
func (a *Agent) connectToServer(ctx context.Context) error {
	a.log.Info("falling back to direct server connection", "server", a.cfg.ServerAddr)

	conn, err := grpc.NewClient(a.cfg.ServerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                20 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return fmt.Errorf("connect to server: %w", err)
	}
	a.upstreamConn = conn

	client := pb.NewDirQServerClient(conn)
	stream, err := client.AgentStream(ctx)
	if err != nil {
		return fmt.Errorf("open server stream: %w", err)
	}
	a.upstreamStream = stream

	// Send Hello — the server rejects streams that don't start with Hello.
	if err := a.sendHello(); err != nil {
		return fmt.Errorf("send hello on fallback: %w", err)
	}

	return nil
}

func (a *Agent) mainLoop(ctx context.Context) error {
	// Start heartbeat ticker.
	heartbeatTicker := time.NewTicker(30 * time.Second)
	defer heartbeatTicker.Stop()

	// Start receiving from upstream in a goroutine.
	msgCh := make(chan *pb.ServerMessage, 16)
	errCh := make(chan error, 1)
	go func() {
		for {
			msg, err := a.upstreamStream.Recv()
			if err == io.EOF {
				errCh <- nil
				return
			}
			if err != nil {
				errCh <- err
				return
			}
			msgCh <- msg
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-errCh:
			return fmt.Errorf("upstream stream error: %w", err)

		case <-heartbeatTicker.C:
			a.sendHeartbeat()

		case msg := <-msgCh:
			a.handleServerMessage(ctx, msg)
		}
	}
}

func (a *Agent) sendHeartbeat() {
	a.mu.RLock()
	peerCount := uint32(len(a.downstreams))
	a.mu.RUnlock()

	err := a.upstreamStream.Send(&pb.AgentMessage{
		Payload: &pb.AgentMessage_Heartbeat{
			Heartbeat: &pb.Heartbeat{
				AgentId:        a.agentID,
				Timestamp:      timestamppb.Now(),
				ConnectedPeers: peerCount,
			},
		},
	})
	if err != nil {
		a.log.Error("heartbeat send failed", "error", err)
	}
}

func (a *Agent) handleServerMessage(ctx context.Context, msg *pb.ServerMessage) {
	switch p := msg.Payload.(type) {
	case *pb.ServerMessage_QueryRequest:
		// Queries are broadcast — execute locally AND relay to downstream peers.
		go a.executeQuery(ctx, p.QueryRequest)
	case *pb.ServerMessage_PeerUpdate:
		a.log.Info("peer update received", "new_role", p.PeerUpdate.NewRole)
	case *pb.ServerMessage_UpdatePush:
		a.log.Info("update push received", "version", p.UpdatePush.Version)
	case *pb.ServerMessage_ExecRequest:
		// Exec is targeted — if it's for us, execute. Otherwise relay downstream.
		if p.ExecRequest.AgentId == a.agentID {
			go a.handleExecRequest(ctx, p.ExecRequest)
		} else {
			a.relayToDownstreams(msg)
		}
	case *pb.ServerMessage_PutFile:
		if p.PutFile.AgentId == a.agentID {
			go a.handlePutFile(ctx, p.PutFile)
		} else {
			a.relayToDownstreams(msg)
		}
	case *pb.ServerMessage_FetchFile:
		if p.FetchFile.AgentId == a.agentID {
			go a.handleFetchFile(ctx, p.FetchFile)
		} else {
			a.relayToDownstreams(msg)
		}
	}
}

func (a *Agent) executeQuery(ctx context.Context, qr *pb.QueryRequest) {
	// Always relay to downstream peers first (they may be targets even if we're not).
	a.relayQueryToDownstreams(qr)

	// Check if this agent is a target. If target_agent_ids is empty, it's a
	// broadcast (all agents execute). If populated, only listed agents execute.
	if len(qr.TargetAgentIds) > 0 && !a.isTargeted(qr.TargetAgentIds) {
		return // not a target — just relay, don't execute
	}

	a.log.Info("executing query", "query_id", qr.QueryId, "modules", qr.Modules)

	hostname, _ := os.Hostname()

	// Collect data from requested modules.
	collected := modules.CollectModules(qr.Modules)

	// Apply agent-side filtering (array-aware: filters into packages, services, etc.)
	if len(qr.Filters) > 0 {
		conds := protoFiltersToConditions(qr.Filters)
		collected = query.FilterCollectedData(conds, collected)

		// Check if all WHERE-referenced modules survived filtering.
		// If a module was referenced in a condition but is missing from the
		// result, this agent didn't match the query — don't send a response.
		if !query.AllFilteredModulesPresent(conds, collected) {
			return
		}
	}

	data, err := structpb.NewStruct(collected)
	if err != nil {
		a.log.Error("failed to create struct", "error", err)
		a.sendQueryResult(qr.QueryId, hostname, false, err.Error(), nil)
		return
	}

	a.sendQueryResult(qr.QueryId, hostname, true, "", data)
}

// isTargeted checks if this agent's ID is in the target list.
func (a *Agent) isTargeted(targetIDs []string) bool {
	for _, id := range targetIDs {
		if id == a.agentID {
			return true
		}
	}
	return false
}

func (a *Agent) sendQueryResult(queryID, hostname string, success bool, errMsg string, data *structpb.Struct) {
	result := &pb.AgentMessage{
		Payload: &pb.AgentMessage_QueryResult{
			QueryResult: &pb.QueryResult{
				QueryId:     queryID,
				AgentId:     a.agentID,
				Hostname:    hostname,
				Success:     success,
				Error:       errMsg,
				Data:        data,
				CollectedAt: timestamppb.Now(),
			},
		},
	}

	if err := a.upstreamStream.Send(result); err != nil {
		a.log.Error("failed to send query result", "error", err)
	}
}

func (a *Agent) relayQueryToDownstreams(qr *pb.QueryRequest) {
	a.relayToDownstreams(&pb.ServerMessage{
		Payload: &pb.ServerMessage_QueryRequest{
			QueryRequest: qr,
		},
	})
}

// relayToDownstreams sends any ServerMessage to all connected downstream peers.
func (a *Agent) relayToDownstreams(msg *pb.ServerMessage) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	for _, ds := range a.downstreams {
		select {
		case ds.send <- msg:
		default:
			a.log.Warn("downstream send buffer full", "peer", ds.agentID)
		}
	}
}

// ─────────────────────────────────────────────────────────
// Relay server (for downstream peers)
// ─────────────────────────────────────────────────────────

func (a *Agent) startRelayServer(ctx context.Context) {
	a.grpcSv = grpc.NewServer()
	pb.RegisterDirQRelayServer(a.grpcSv, a)

	lis, err := net.Listen("tcp", a.cfg.ListenAddr)
	if err != nil {
		a.log.Error("relay listen failed", "addr", a.cfg.ListenAddr, "error", err)
		return
	}
	a.log.Info("relay server listening", "addr", a.cfg.ListenAddr)

	go func() {
		<-ctx.Done()
		a.grpcSv.GracefulStop()
	}()

	if err := a.grpcSv.Serve(lis); err != nil {
		a.log.Error("relay server error", "error", err)
	}
}

// RelayStream handles a downstream peer connecting to this agent.
func (a *Agent) RelayStream(stream pb.DirQRelay_RelayStreamServer) error {
	// First message must be a Hello.
	msg, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := msg.GetHello()
	if hello == nil {
		return fmt.Errorf("first message must be AgentHello")
	}

	peerID := hello.AgentId
	a.log.Info("downstream peer connected", "peer_id", peerID)

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	ds := &downstreamPeer{
		agentID: peerID,
		send:    make(chan *pb.ServerMessage, 64),
		cancel:  cancel,
	}

	a.mu.Lock()
	a.downstreams[peerID] = ds
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.downstreams, peerID)
		a.mu.Unlock()
		a.log.Info("downstream peer disconnected", "peer_id", peerID)
	}()

	// Sender goroutine
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-ds.send:
				if err := stream.Send(msg); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Receiver loop — forward ALL responses from downstream peers upstream.
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		// Forward everything upstream (query results, exec responses, file chunks).
		if err := a.upstreamStream.Send(msg); err != nil {
			a.log.Error("failed to relay upstream", "peer_id", peerID, "error", err)
			return err
		}
	}
}

// ─────────────────────────────────────────────────────────
// Agent-side filtering
// ─────────────────────────────────────────────────────────

// protoFiltersToConditions converts proto Filter messages back into query.Condition
// objects so the agent can use the query package's FilterCollectedData.
func protoFiltersToConditions(filters []*pb.Filter) []*query.Condition {
	conds := make([]*query.Condition, 0, len(filters))
	for _, f := range filters {
		if f.Operator == "IN" {
			// IN operator: value is comma-separated list.
			values := strings.Split(f.Value, ",")
			conds = append(conds, &query.Condition{
				Field: f.Field,
				In:    &query.InClause{Values: values},
			})
		} else {
			c := &query.Condition{
				Field:    f.Field,
				Operator: f.Operator,
			}
			// Try to parse as number, fall back to string.
			if num, err := strconv.ParseFloat(f.Value, 64); err == nil {
				c.Value = &query.Value{Number: &num}
			} else {
				val := f.Value
				c.Value = &query.Value{String: &val}
			}
			conds = append(conds, c)
		}
	}
	return conds
}
