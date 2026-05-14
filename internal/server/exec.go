// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/query"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// ─────────────────────────────────────────────────────────
// Exec session tracking (same pattern as query dispatch)
// ─────────────────────────────────────────────────────────

type execSession struct {
	requestID string
	result    chan any // receives *pb.ExecResponse, *pb.FileChunk, or *pb.FetchFileResponse
	startedAt time.Time
	timeout   time.Duration
}

// ─────────────────────────────────────────────────────────
// Server-side exec handlers (from gRPC stream)
// ─────────────────────────────────────────────────────────

func (s *Server) handleExecResponse(resp *pb.ExecResponse) {
	s.execMu.RLock()
	es, ok := s.execSessions[resp.RequestId]
	s.execMu.RUnlock()

	if ok {
		select {
		case es.result <- resp:
		default:
			s.log.Warn("exec result channel full", "request_id", resp.RequestId)
		}
	}

	// Update audit log.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rc := int(resp.Rc)
		finishedAt := time.Now()
		if resp.FinishedAt != nil {
			finishedAt = resp.FinishedAt.AsTime()
		}
		errMsg := resp.Error
		s.db.UpdateExecLog(ctx, resp.RequestId, &rc, resp.Success, errMsg, finishedAt)
	}()
}

func (s *Server) handleFileChunk(chunk *pb.FileChunk) {
	s.execMu.RLock()
	es, ok := s.execSessions[chunk.RequestId]
	s.execMu.RUnlock()

	if ok {
		select {
		case es.result <- chunk:
		default:
		}
	}
}

func (s *Server) handleFetchResponse(resp *pb.FetchFileResponse) {
	s.execMu.RLock()
	es, ok := s.execSessions[resp.RequestId]
	s.execMu.RUnlock()

	if ok {
		select {
		case es.result <- resp:
		default:
		}
	}
}

// ─────────────────────────────────────────────────────────
// Dispatch exec to a single agent and wait for response
// ─────────────────────────────────────────────────────────

func (s *Server) dispatchExec(ctx context.Context, agentID string, msg *pb.ServerMessage, requestID string, timeout time.Duration) (any, error) {
	es := &execSession{
		requestID: requestID,
		result:    make(chan any, 1),
		startedAt: time.Now(),
		timeout:   timeout,
	}

	s.execMu.Lock()
	s.execSessions[requestID] = es
	s.execMu.Unlock()

	defer func() {
		s.execMu.Lock()
		delete(s.execSessions, requestID)
		s.execMu.Unlock()
	}()

	if err := s.signServerMessage(msg); err != nil {
		return nil, fmt.Errorf("sign control message: %w", err)
	}

	// Find the stream to send through. If the target agent is a zone
	// leader, send directly. Otherwise, find its zone leader ancestor
	// and route through that zone leader's stream — the mesh relays it.
	s.mu.RLock()
	as, ok := s.streams[agentID]
	s.mu.RUnlock()

	if !ok {
		// Agent is not directly connected (it's a relay or leaf).
		// Find its zone leader and route through that stream.
		zl, err := s.db.FindZoneLeader(ctx, agentID)
		if err != nil {
			return nil, fmt.Errorf("agent %s not connected and no zone leader found: %w", agentID, err)
		}
		s.mu.RLock()
		as, ok = s.streams[zl.ID]
		s.mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("zone leader %s for agent %s is not connected", zl.ID, agentID)
		}
		s.log.Info("routing exec through zone leader",
			"target", agentID, "zone_leader", zl.ID, "zone_leader_host", zl.Hostname)
	}

	select {
	case as.send <- msg:
	default:
		return nil, fmt.Errorf("send buffer full for stream handling agent %s", agentID)
	}

	// Wait for response.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case result := <-es.result:
		return result, nil
	case <-timer.C:
		return nil, fmt.Errorf("exec timeout after %s", timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ─────────────────────────────────────────────────────────
// REST API: exec_command
// ─────────────────────────────────────────────────────────

type execCommandRequest struct {
	AgentID      string            `json:"agent_id"`
	Command      string            `json:"command"`
	Stdin        string            `json:"stdin"`        // base64-encoded stdin data
	Become       bool              `json:"become"`
	BecomeUser   string            `json:"become_user"`
	BecomeMethod string            `json:"become_method"`
	Environment  map[string]string `json:"environment"`
	Timeout      int               `json:"timeout"`
	// AAP attribution
	AAPJobID       string `json:"aap_job_id"`
	AAPJobTemplate string `json:"aap_job_template"`
	AAPUser        string `json:"aap_user"`
}

type execCommandResponse struct {
	RequestID  string `json:"request_id"`
	AgentID    string `json:"agent_id"`
	Hostname   string `json:"hostname"`
	RC         int    `json:"rc"`
	Stdout     string `json:"stdout"`          // base64-encoded
	Stderr     string `json:"stderr"`          // base64-encoded
	Success    bool   `json:"success"`
	Error      string `json:"error,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

func (s *Server) handleExecCommand(w http.ResponseWriter, r *http.Request) {
	var req execCommandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.AgentID == "" || req.Command == "" {
		httpError(w, http.StatusBadRequest, "agent_id and command are required")
		return
	}

	// Verify agent exists and has exec enabled.
	agent, err := s.db.GetAgent(r.Context(), req.AgentID)
	if err != nil {
		httpError(w, http.StatusNotFound, "agent not found")
		return
	}
	if !agent.ExecEnabled {
		httpError(w, http.StatusForbidden, "exec is not enabled on this agent")
		return
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = 300
	}

	requestID := fmt.Sprintf("exec-%d", time.Now().UnixNano())

	// Create audit log entry.
	cmd := req.Command
	s.db.CreateExecLog(r.Context(), db.ExecLog{
		RequestID:      requestID,
		AgentID:        req.AgentID,
		Hostname:       agent.Hostname,
		Operation:      "exec_command",
		Command:        &cmd,
		Become:         req.Become,
		BecomeUser:     strPtr(req.BecomeUser),
		AAPJobID:       strPtr(req.AAPJobID),
		AAPJobTemplate: strPtr(req.AAPJobTemplate),
		AAPUser:        strPtr(req.AAPUser),
		StartedAt:      timePtr(time.Now()),
	})

	// Decode stdin if provided (base64-encoded from the REST API).
	var stdinBytes []byte
	if req.Stdin != "" {
		var decErr error
		stdinBytes, decErr = decodeBase64(req.Stdin)
		if decErr != nil {
			httpError(w, http.StatusBadRequest, "invalid base64 stdin: "+decErr.Error())
			return
		}
	}

	// Build proto request.
	pbReq := &pb.ServerMessage{
		Payload: &pb.ServerMessage_ExecRequest{
			ExecRequest: &pb.ExecRequest{
				RequestId:      requestID,
				AgentId:        req.AgentID,
				Command:        req.Command,
				Stdin:          stdinBytes,
				Become:         req.Become,
				BecomeUser:     req.BecomeUser,
				BecomeMethod:   req.BecomeMethod,
				Environment:    req.Environment,
				TimeoutSeconds: int32(timeout),
				AapJobId:       req.AAPJobID,
				AapJobTemplate: req.AAPJobTemplate,
				AapUser:        req.AAPUser,
			},
		},
	}

	result, err := s.dispatchExec(r.Context(), req.AgentID, pbReq, requestID, time.Duration(timeout)*time.Second)
	if err != nil {
		httpError(w, http.StatusGatewayTimeout, "exec failed: "+err.Error())
		return
	}

	resp, ok := result.(*pb.ExecResponse)
	if !ok {
		httpError(w, http.StatusInternalServerError, "unexpected response type")
		return
	}

	out := execCommandResponse{
		RequestID: resp.RequestId,
		AgentID:   resp.AgentId,
		Hostname:  resp.Hostname,
		RC:        int(resp.Rc),
		Stdout:    encodeBase64(resp.Stdout),
		Stderr:    encodeBase64(resp.Stderr),
		Success:   resp.Success,
		Error:     resp.Error,
	}
	if resp.StartedAt != nil {
		out.StartedAt = resp.StartedAt.AsTime().Format(time.RFC3339)
	}
	if resp.FinishedAt != nil {
		out.FinishedAt = resp.FinishedAt.AsTime().Format(time.RFC3339)
	}

	jsonResponse(w, http.StatusOK, out)
}

// ─────────────────────────────────────────────────────────
// REST API: put_file
// ─────────────────────────────────────────────────────────

type putFileRequest struct {
	AgentID    string `json:"agent_id"`
	DestPath   string `json:"dest_path"`
	Content    string `json:"content"` // base64-encoded file content
	Mode       int    `json:"mode"`    // unix file mode
	Become     bool   `json:"become"`
	BecomeUser string `json:"become_user"`
	Timeout    int    `json:"timeout"`
	// AAP attribution
	AAPJobID       string `json:"aap_job_id"`
	AAPJobTemplate string `json:"aap_job_template"`
	AAPUser        string `json:"aap_user"`
}

func (s *Server) handlePutFile(w http.ResponseWriter, r *http.Request) {
	var req putFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.AgentID == "" || req.DestPath == "" {
		httpError(w, http.StatusBadRequest, "agent_id and dest_path are required")
		return
	}

	agent, err := s.db.GetAgent(r.Context(), req.AgentID)
	if err != nil {
		httpError(w, http.StatusNotFound, "agent not found")
		return
	}
	if !agent.ExecEnabled {
		httpError(w, http.StatusForbidden, "exec is not enabled on this agent")
		return
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = 300 // 5 minutes for file transfers
	}

	requestID := fmt.Sprintf("put-%d", time.Now().UnixNano())

	// Decode base64 content.
	content, err := decodeBase64(req.Content)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid base64 content: "+err.Error())
		return
	}

	// Audit log.
	s.db.CreateExecLog(r.Context(), db.ExecLog{
		RequestID:      requestID,
		AgentID:        req.AgentID,
		Hostname:       agent.Hostname,
		Operation:      "put_file",
		DestPath:       &req.DestPath,
		Become:         req.Become,
		BecomeUser:     strPtr(req.BecomeUser),
		AAPJobID:       strPtr(req.AAPJobID),
		AAPJobTemplate: strPtr(req.AAPJobTemplate),
		AAPUser:        strPtr(req.AAPUser),
		StartedAt:      timePtr(time.Now()),
	})

	pbReq := &pb.ServerMessage{
		Payload: &pb.ServerMessage_PutFile{
			PutFile: &pb.PutFileRequest{
				RequestId:      requestID,
				AgentId:        req.AgentID,
				DestPath:       req.DestPath,
				Content:        content,
				Mode:           int32(req.Mode),
				Become:         req.Become,
				BecomeUser:     req.BecomeUser,
				AapJobId:       req.AAPJobID,
				AapJobTemplate: req.AAPJobTemplate,
				AapUser:        req.AAPUser,
			},
		},
	}

	result, err := s.dispatchExec(r.Context(), req.AgentID, pbReq, requestID, time.Duration(timeout)*time.Second)
	if err != nil {
		httpError(w, http.StatusGatewayTimeout, "put_file failed: "+err.Error())
		return
	}

	chunk, ok := result.(*pb.FileChunk)
	if !ok {
		httpError(w, http.StatusInternalServerError, "unexpected response type")
		return
	}

	// Update audit log.
	now := time.Now()
	s.db.UpdateExecLog(r.Context(), requestID, nil, chunk.Success, chunk.Error, now)

	jsonResponse(w, http.StatusOK, map[string]any{
		"request_id": chunk.RequestId,
		"success":    chunk.Success,
		"error":      chunk.Error,
	})
}

// ─────────────────────────────────────────────────────────
// REST API: fetch_file
// ─────────────────────────────────────────────────────────

type fetchFileRequest struct {
	AgentID    string `json:"agent_id"`
	SrcPath    string `json:"src_path"`
	Become     bool   `json:"become"`
	BecomeUser string `json:"become_user"`
	Timeout    int    `json:"timeout"`
	// AAP attribution
	AAPJobID       string `json:"aap_job_id"`
	AAPJobTemplate string `json:"aap_job_template"`
	AAPUser        string `json:"aap_user"`
}

func (s *Server) handleFetchFile(w http.ResponseWriter, r *http.Request) {
	var req fetchFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.AgentID == "" || req.SrcPath == "" {
		httpError(w, http.StatusBadRequest, "agent_id and src_path are required")
		return
	}

	agent, err := s.db.GetAgent(r.Context(), req.AgentID)
	if err != nil {
		httpError(w, http.StatusNotFound, "agent not found")
		return
	}
	if !agent.ExecEnabled {
		httpError(w, http.StatusForbidden, "exec is not enabled on this agent")
		return
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = 300
	}

	requestID := fmt.Sprintf("fetch-%d", time.Now().UnixNano())

	// Audit log.
	s.db.CreateExecLog(r.Context(), db.ExecLog{
		RequestID:      requestID,
		AgentID:        req.AgentID,
		Hostname:       agent.Hostname,
		Operation:      "fetch_file",
		SrcPath:        &req.SrcPath,
		Become:         req.Become,
		BecomeUser:     strPtr(req.BecomeUser),
		AAPJobID:       strPtr(req.AAPJobID),
		AAPJobTemplate: strPtr(req.AAPJobTemplate),
		AAPUser:        strPtr(req.AAPUser),
		StartedAt:      timePtr(time.Now()),
	})

	pbReq := &pb.ServerMessage{
		Payload: &pb.ServerMessage_FetchFile{
			FetchFile: &pb.FetchFileRequest{
				RequestId:      requestID,
				AgentId:        req.AgentID,
				SrcPath:        req.SrcPath,
				Become:         req.Become,
				BecomeUser:     req.BecomeUser,
				AapJobId:       req.AAPJobID,
				AapJobTemplate: req.AAPJobTemplate,
				AapUser:        req.AAPUser,
			},
		},
	}

	result, err := s.dispatchExec(r.Context(), req.AgentID, pbReq, requestID, time.Duration(timeout)*time.Second)
	if err != nil {
		httpError(w, http.StatusGatewayTimeout, "fetch_file failed: "+err.Error())
		return
	}

	resp, ok := result.(*pb.FetchFileResponse)
	if !ok {
		httpError(w, http.StatusInternalServerError, "unexpected response type")
		return
	}

	// Update audit log.
	now := time.Now()
	s.db.UpdateExecLog(r.Context(), requestID, nil, resp.Success, resp.Error, now)

	if !resp.Success {
		httpError(w, http.StatusInternalServerError, resp.Error)
		return
	}

	b64Content := encodeBase64(resp.Content)

	jsonResponse(w, http.StatusOK, map[string]any{
		"request_id": resp.RequestId,
		"success":    resp.Success,
		"content":    b64Content,
		"mode":       resp.Mode,
		"size":       resp.Size,
	})
}

// ─────────────────────────────────────────────────────────
// REST API: exec audit log
// ─────────────────────────────────────────────────────────

func (s *Server) handleListExecLogs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	agentID := r.URL.Query().Get("agent_id")
	jobID := r.URL.Query().Get("aap_job_id")

	ctx := r.Context()
	var logs []db.ExecLog
	var err error

	if jobID != "" {
		logs, err = s.db.ListExecLogsByJob(ctx, jobID)
	} else if agentID != "" {
		logs, err = s.db.ListExecLogsByAgent(ctx, agentID, limit)
	} else {
		logs, err = s.db.ListExecLogs(ctx, limit)
	}

	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, logs)
}

// ─────────────────────────────────────────────────────────
// REST API: exec_multi (broadcast exec to multiple agents)
// ─────────────────────────────────────────────────────────

type execMultiRequest struct {
	Query        string            `json:"query"`
	Command      string            `json:"command"`
	Script       string            `json:"script"`       // base64-encoded script content
	ScriptName   string            `json:"script_name"`  // original filename (e.g. "deploy.sh")
	Stdin        string            `json:"stdin"`         // base64-encoded stdin data
	Become       bool              `json:"become"`
	BecomeUser   string            `json:"become_user"`
	BecomeMethod string            `json:"become_method"`
	Environment  map[string]string `json:"environment"`
	Timeout      int               `json:"timeout"`
}

// execMultiResult is each NDJSON line streamed as agents respond.
type execMultiResult struct {
	Type       string `json:"type"`
	RequestID  string `json:"request_id,omitempty"`
	AgentID    string `json:"agent_id,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
	RC         int    `json:"rc,omitempty"`
	Stdout     string `json:"stdout,omitempty"`          // base64-encoded
	Stderr     string `json:"stderr,omitempty"`          // base64-encoded
	Success    bool   `json:"success"`
	Error      string `json:"error,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	TotalTargets int  `json:"total_targets,omitempty"`
}

// execBroadcastSession tracks an in-flight broadcast exec.
type execBroadcastSession struct {
	requestID string
	results   chan *pb.ExecResponse
	startedAt time.Time
	timeout   time.Duration
}

var (
	execBroadcastSessions   = make(map[string]*execBroadcastSession)
	execBroadcastSessionsMu sync.RWMutex
)

func (s *Server) handleExecBroadcastResponse(resp *pb.ExecResponse) {
	// Check broadcast sessions first.
	execBroadcastSessionsMu.RLock()
	bs, ok := execBroadcastSessions[resp.RequestId]
	execBroadcastSessionsMu.RUnlock()

	if ok {
		select {
		case bs.results <- resp:
		default:
			s.log.Warn("broadcast exec result channel full", "request_id", resp.RequestId)
		}
		return
	}

	// Fall through to single-agent exec session handling.
	s.handleExecResponse(resp)
}

func (s *Server) handleExecMulti(w http.ResponseWriter, r *http.Request) {
	var req execMultiRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.Command == "" && req.Script == "" {
		httpError(w, http.StatusBadRequest, "command or script is required")
		return
	}
	if req.Query == "" {
		httpError(w, http.StatusBadRequest, "query is required (used to select target agents)")
		return
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = 300
	}

	// Decode stdin if provided.
	var stdinBytes []byte
	if req.Stdin != "" {
		var decErr error
		stdinBytes, decErr = decodeBase64(req.Stdin)
		if decErr != nil {
			httpError(w, http.StatusBadRequest, "invalid base64 stdin: "+decErr.Error())
			return
		}
	}

	// Decode script if provided.
	var scriptBytes []byte
	if req.Script != "" {
		var decErr error
		scriptBytes, decErr = decodeBase64(req.Script)
		if decErr != nil {
			httpError(w, http.StatusBadRequest, "invalid base64 script: "+decErr.Error())
			return
		}
	}

	// Resolve target agents using the query engine.
	ctx := r.Context()
	parsed, err := query.Parse(req.Query)
	if err != nil {
		httpError(w, http.StatusBadRequest, "query parse error: "+err.Error())
		return
	}

	online := true
	allAgents, err := s.db.ListAgents(ctx, db.ListAgentsFilter{Online: &online})
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to list agents: "+err.Error())
		return
	}

	// Pre-filter by tag conditions.
	agents := allAgents
	if query.HasTagConditions(parsed.Where) {
		agents = make([]db.Agent, 0, len(allAgents))
		for _, a := range allAgents {
			if query.MatchesAgentTags(parsed.Where, a.Tags) {
				agents = append(agents, a)
			}
		}
	}

	// Filter to agents with exec enabled.
	var targets []db.Agent
	for _, a := range agents {
		if a.ExecEnabled {
			targets = append(targets, a)
		}
	}

	// Set up streaming.
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)

	// Header line.
	enc.Encode(execMultiResult{
		Type:         "header",
		TotalTargets: len(targets),
	})
	flusher.Flush()

	if len(targets) == 0 {
		return
	}

	// Build target ID list.
	targetIDs := make([]string, len(targets))
	for i, a := range targets {
		targetIDs[i] = a.ID
	}

	requestID := fmt.Sprintf("execm-%d", time.Now().UnixNano())

	// Create broadcast session to collect responses.
	bs := &execBroadcastSession{
		requestID: requestID,
		results:   make(chan *pb.ExecResponse, len(targets)),
		startedAt: time.Now(),
		timeout:   time.Duration(timeout) * time.Second,
	}

	execBroadcastSessionsMu.Lock()
	execBroadcastSessions[requestID] = bs
	execBroadcastSessionsMu.Unlock()

	defer func() {
		execBroadcastSessionsMu.Lock()
		delete(execBroadcastSessions, requestID)
		execBroadcastSessionsMu.Unlock()
	}()

	// Build one broadcast message with all target IDs.
	msg := &pb.ServerMessage{
		Payload: &pb.ServerMessage_ExecRequest{
			ExecRequest: &pb.ExecRequest{
				RequestId:      requestID,
				TargetAgentIds: targetIDs,
				Command:        req.Command,
				Stdin:          stdinBytes,
				Script:         scriptBytes,
				ScriptName:     req.ScriptName,
				Become:         req.Become,
				BecomeUser:     req.BecomeUser,
				BecomeMethod:   req.BecomeMethod,
				Environment:    req.Environment,
				TimeoutSeconds: int32(timeout),
			},
		},
	}

	if err := s.signServerMessage(msg); err != nil {
		enc.Encode(execMultiResult{
			Type:    "result",
			Success: false,
			Error:   "sign failed: " + err.Error(),
		})
		flusher.Flush()
		return
	}

	// Broadcast to all zone leaders (same as query dispatch).
	sent := 0
	s.mu.RLock()
	for _, as := range s.streams {
		select {
		case as.send <- msg:
			sent++
		default:
			s.log.Warn("zone leader send buffer full during exec broadcast", "agent_id", as.agentID)
		}
	}
	s.mu.RUnlock()

	s.log.Info("exec broadcast sent",
		"request_id", requestID,
		"targets", len(targetIDs),
		"zone_leaders", sent,
	)

	// Stream results as agents respond.
	hardTimeout := time.NewTimer(bs.timeout)
	defer hardTimeout.Stop()
	idleTimeout := time.NewTimer(30 * time.Second)
	defer idleTimeout.Stop()

	received := 0
	for received < len(targets) {
		select {
		case resp := <-bs.results:
			received++
			out := execMultiResult{
				Type:      "result",
				RequestID: resp.RequestId,
				AgentID:   resp.AgentId,
				Hostname:  resp.Hostname,
				RC:        int(resp.Rc),
				Stdout:    encodeBase64(resp.Stdout),
				Stderr:    encodeBase64(resp.Stderr),
				Success:   resp.Success,
				Error:     resp.Error,
			}
			if resp.StartedAt != nil {
				out.StartedAt = resp.StartedAt.AsTime().Format(time.RFC3339)
			}
			if resp.FinishedAt != nil {
				out.FinishedAt = resp.FinishedAt.AsTime().Format(time.RFC3339)
			}
			enc.Encode(out)
			flusher.Flush()

			if !idleTimeout.Stop() {
				select {
				case <-idleTimeout.C:
				default:
				}
			}
			idleTimeout.Reset(30 * time.Second)
		case <-idleTimeout.C:
			return
		case <-hardTimeout.C:
			s.log.Warn("exec broadcast timed out", "request_id", requestID, "received", received, "targets", len(targets))
			return
		case <-ctx.Done():
			return
		}
	}
}

// ─────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func timePtr(t time.Time) *time.Time {
	return &t
}

func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

func encodeBase64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
