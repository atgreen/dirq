// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"bytes"
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
	// targetAgentID is the agent this request was dispatched to. Responses
	// are correlated on (request_id, agent_id): the request id alone is not
	// an authorization, and any agent that sees the request — which, with
	// fan-out routing, is most of them — could otherwise answer for the
	// target (dirq-632.2).
	targetAgentID string
	result        chan any // receives *pb.ExecResponse, *pb.FileChunk, or *pb.FetchFileResponse
	startedAt     time.Time
	timeout       time.Duration
}

// ─────────────────────────────────────────────────────────
// Server-side exec handlers (from gRPC stream)
// ─────────────────────────────────────────────────────────

func (s *Server) handleExecResponse(origin string, resp *pb.ExecResponse) {
	// Checked before the session lookup so a rejected response leaves no
	// trace anywhere — including the audit log below, which is otherwise
	// written even for a response no session was waiting on.
	if !s.originAllows(origin, resp.AgentId, "exec_response") {
		return
	}

	s.execMu.RLock()
	es, ok := s.execSessions[resp.RequestId]
	s.execMu.RUnlock()

	if ok {
		if s.originTargetMismatch(es.targetAgentID, resp.AgentId, "exec_response") {
			return
		}
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

func (s *Server) handleFileChunk(origin string, chunk *pb.FileChunk) {
	s.execMu.RLock()
	es, ok := s.execSessions[chunk.RequestId]
	s.execMu.RUnlock()

	if ok {
		if s.originTargetMismatch(es.targetAgentID, chunk.AgentId, "file_chunk") {
			return
		}
		if !s.originAllows(origin, chunk.AgentId, "file_chunk") {
			return
		}
		select {
		case es.result <- chunk:
		default:
		}
	}
}

func (s *Server) handleFetchResponse(origin string, resp *pb.FetchFileResponse) {
	s.execMu.RLock()
	es, ok := s.execSessions[resp.RequestId]
	s.execMu.RUnlock()

	if ok {
		if s.originTargetMismatch(es.targetAgentID, resp.AgentId, "fetch_response") {
			return
		}
		if !s.originAllows(origin, resp.AgentId, "fetch_response") {
			return
		}
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
		requestID:     requestID,
		targetAgentID: agentID,
		result:        make(chan any, 1),
		startedAt:     time.Now(),
		timeout:       timeout,
	}

	s.execMu.Lock()
	s.execSessions[requestID] = es
	s.execMu.Unlock()

	defer func() {
		s.execMu.Lock()
		delete(s.execSessions, requestID)
		s.execMu.Unlock()
	}()

	// Routing. This message is addressed to one agent, so it should travel
	// the one path that reaches it: the payload is a command line, a script,
	// a file body, and every agent that relays it can read it (dirq-632.1).
	//
	// The route is decided before signing, because relay_path rides inside
	// the signed envelope — a relay can drop or delay a message but it
	// cannot redirect one.
	s.mu.RLock()
	as, directlyConnected := s.streams[agentID]
	s.mu.RUnlock()

	var route *agentStream
	switch {
	case directlyConnected:
		// A zone leader: one hop, no relaying, no path needed.
		route = as
	default:
		if path := s.topology.PathFromZoneLeader(agentID); len(path) > 0 {
			s.mu.RLock()
			zl, ok := s.streams[path[0]]
			s.mu.RUnlock()
			if ok && setRelayPath(msg, path) {
				route = zl
			}
		}
	}

	if err := s.signServerMessage(msg); err != nil {
		return nil, fmt.Errorf("sign control message: %w", err)
	}

	if route != nil {
		select {
		case route.send <- msg:
		default:
			return nil, fmt.Errorf("send buffer full for stream handling agent %s", agentID)
		}
	} else {
		// No traceable route: the topology does not know this agent, or the
		// zone leader that owns it has no live stream. Fall back to the old
		// behaviour — fan out to every connected agent and let the target
		// recognize itself — rather than failing an exec because the tree
		// moved. The payload is exposed fleet-wide while this lasts, so it
		// is worth knowing how often it happens.
		metricExecFanout.Inc()
		sent := 0
		s.mu.RLock()
		for _, peer := range s.streams {
			select {
			case peer.send <- msg:
				sent++
			default:
				s.log.Warn("send buffer full during exec fan-out", "agent_id", peer.agentID)
			}
		}
		s.mu.RUnlock()
		if sent == 0 {
			return nil, fmt.Errorf("no connected agents to relay exec to %s", agentID)
		}
		s.log.Warn("no route to agent — falling back to fleet-wide fan-out",
			"target", agentID, "relays", sent)
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

// setRelayPath records the route on the request inside msg, reporting whether
// this is a message kind that carries one. Only the single-agent operations do
// — a broadcast has many targets and no single path — and the field lives on
// the request rather than the envelope because a oneof member is marshalled
// last, so nothing added to the envelope can sort after it without breaking
// signature verification on agents that predate the field (see the note in
// dirq.proto and signutil's TestRelayPathSurvivesAnOlderAgent).
func setRelayPath(msg *pb.ServerMessage, path []string) bool {
	switch p := msg.Payload.(type) {
	case *pb.ServerMessage_ExecRequest:
		p.ExecRequest.RelayPath = path
	case *pb.ServerMessage_PutFile:
		p.PutFile.RelayPath = path
	case *pb.ServerMessage_FetchFile:
		p.FetchFile.RelayPath = path
	default:
		return false
	}
	return true
}

// ─────────────────────────────────────────────────────────
// REST API: exec_command
// ─────────────────────────────────────────────────────────

type execCommandRequest struct {
	AgentID      string            `json:"agent_id"`
	Command      string            `json:"command"`
	Stdin        string            `json:"stdin"` // base64-encoded stdin data
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
	Stdout     string `json:"stdout"` // base64-encoded
	Stderr     string `json:"stderr"` // base64-encoded
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

	if err := s.bindAAP(r, req.AAPUser); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
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

	if err := s.bindAAP(r, req.AAPUser); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
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

	if err := s.bindAAP(r, req.AAPUser); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
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
	Script       string            `json:"script"`      // base64-encoded script content
	ScriptName   string            `json:"script_name"` // original filename (e.g. "deploy.sh")
	Stdin        string            `json:"stdin"`       // base64-encoded stdin data
	Become       bool              `json:"become"`
	BecomeUser   string            `json:"become_user"`
	BecomeMethod string            `json:"become_method"`
	Environment  map[string]string `json:"environment"`
	Timeout      int               `json:"timeout"`
	// BottomUp dispatches targets deepest-mesh-depth first, one wave per
	// depth, so a relay is never executed while an agent beneath it is still
	// running (e.g. `reboot`). Off = the historical single broadcast.
	BottomUp bool `json:"bottom_up"`
	// AAP attribution
	AAPJobID       string `json:"aap_job_id"`
	AAPJobTemplate string `json:"aap_job_template"`
	AAPUser        string `json:"aap_user"`
}

// execMultiResult is each NDJSON line streamed as agents respond.
type execMultiResult struct {
	Type         string `json:"type"`
	RequestID    string `json:"request_id,omitempty"`
	AgentID      string `json:"agent_id,omitempty"`
	Hostname     string `json:"hostname,omitempty"`
	RC           int    `json:"rc,omitempty"`
	Stdout       string `json:"stdout,omitempty"` // base64-encoded
	Stderr       string `json:"stderr,omitempty"` // base64-encoded
	Success      bool   `json:"success"`
	Error        string `json:"error,omitempty"`
	StartedAt    string `json:"started_at,omitempty"`
	FinishedAt   string `json:"finished_at,omitempty"`
	TotalTargets int    `json:"total_targets,omitempty"`
	Received     int    `json:"received,omitempty"`
	// Targets the field-resolution query couldn't account for.  Non-zero
	// means the broadcast set is a subset of the real matching fleet —
	// callers shouldn't claim "complete" coverage.
	UnresolvedTargets int `json:"unresolved_targets,omitempty"`
}

// execBroadcastSession tracks an in-flight broadcast exec.  The embedded
// *sessionAccounting is the single first-terminal-wins gate for both
// real ExecResponses and synthetic disconnect failures, so the
// dispatcher loop never counts the same agent twice.
type execBroadcastSession struct {
	requestID string
	results   chan *pb.ExecResponse
	startedAt time.Time
	timeout   time.Duration
	*sessionAccounting
}

var (
	execBroadcastSessions   = make(map[string]*execBroadcastSession)
	execBroadcastSessionsMu sync.RWMutex
)

func (s *Server) handleExecBroadcastResponse(origin string, resp *pb.ExecResponse) {
	// Check broadcast sessions first.
	execBroadcastSessionsMu.RLock()
	bs, ok := execBroadcastSessions[resp.RequestId]
	execBroadcastSessionsMu.RUnlock()

	if ok {
		// Origin first: a forged response must not reach ClaimAgent, or it
		// takes the victim's accounting slot and the victim's real answer is
		// then discarded as a duplicate.
		if !s.originAllows(origin, resp.AgentId, "exec_response") {
			return
		}
		// First-terminal-wins gate.  Drop the response if a synthetic
		// disconnect failure already accounted for this agent.
		if bs.ClaimAgent(resp.AgentId) {
			select {
			case bs.results <- resp:
			default:
				s.log.Warn("broadcast exec result channel full", "request_id", resp.RequestId)
			}
		}
		return
	}

	// Fall through to single-agent exec session handling.
	s.handleExecResponse(origin, resp)
}

// decodeExecMultiRequest parses and validates the exec_multi request body,
// decoding the base64 stdin and script fields. On failure it writes the
// HTTP error response and returns ok=false.
func decodeExecMultiRequest(w http.ResponseWriter, r *http.Request) (req execMultiRequest, stdin, script []byte, ok bool) {
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return req, nil, nil, false
	}

	if req.Command == "" && req.Script == "" {
		httpError(w, http.StatusBadRequest, "command or script is required")
		return req, nil, nil, false
	}
	if req.Query == "" {
		httpError(w, http.StatusBadRequest, "query is required (used to select target agents)")
		return req, nil, nil, false
	}

	if req.Stdin != "" {
		var err error
		if stdin, err = decodeBase64(req.Stdin); err != nil {
			httpError(w, http.StatusBadRequest, "invalid base64 stdin: "+err.Error())
			return req, nil, nil, false
		}
	}
	if req.Script != "" {
		var err error
		if script, err = decodeBase64(req.Script); err != nil {
			httpError(w, http.StatusBadRequest, "invalid base64 script: "+err.Error())
			return req, nil, nil, false
		}
	}

	return req, stdin, script, true
}

// resolveBroadcastTargets resolves the exec-enabled online agents matching
// the query: pre-filtered by tag/hostname conditions (resolved server-side
// from the DB), then — when field conditions are present — intersected with
// a resolution query across the fleet. unresolved counts targets the
// resolution query couldn't account for, so the caller's header can surface
// partial coverage.
//
// Shared by the exec and deploy broadcast paths. Deploy used to carry its
// own copy that tested for tag conditions only, so a hostname- or
// field-filtered deploy silently landed on the whole fleet (dirq-8cp).
// One resolver means the two paths cannot drift apart like that again.
func (s *Server) resolveBroadcastTargets(ctx context.Context, queryStr string, parsed *query.Query, timeout int) (targets []db.Agent, unresolved int, err error) {
	online := true
	allAgents, err := s.db.ListAgents(ctx, db.ListAgentsFilter{Online: &online})
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list agents: %w", err)
	}

	agents := allAgents
	if query.HasTagConditions(parsed.Where) || query.HasHostnameCondition(parsed.Where) {
		agents = make([]db.Agent, 0, len(allAgents))
		for _, a := range allAgents {
			if query.MatchesAgentRecord(parsed.Where, a.Tags, a.Hostname) {
				agents = append(agents, a)
			}
		}
	}

	if query.HasFieldConditions(parsed.Where) {
		matchedIDs, missing, err := s.resolveFieldTargets(ctx, queryStr, timeout)
		if err != nil {
			s.log.Warn("field-based target resolution failed, using tag filter only", "error", err)
		} else {
			unresolved = missing
			matched := make(map[string]bool, len(matchedIDs))
			for _, id := range matchedIDs {
				matched[id] = true
			}
			filtered := make([]db.Agent, 0, len(agents))
			for _, a := range agents {
				if matched[a.ID] {
					filtered = append(filtered, a)
				}
			}
			agents = filtered
		}
	}

	for _, a := range agents {
		if a.ExecEnabled {
			targets = append(targets, a)
		}
	}
	return targets, unresolved, nil
}

func (s *Server) handleExecMulti(w http.ResponseWriter, r *http.Request) {
	req, stdinBytes, scriptBytes, ok := decodeExecMultiRequest(w, r)
	if !ok {
		return
	}

	if err := s.bindAAP(r, req.AAPUser); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
		return
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = 300
	}

	// Resolve target agents using the query engine.
	ctx := r.Context()
	parsed, err := query.Parse(req.Query)
	if err != nil {
		httpError(w, http.StatusBadRequest, "query parse error: "+err.Error())
		return
	}

	targets, unresolvedTargets, err := s.resolveBroadcastTargets(ctx, req.Query, parsed, timeout)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
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
		Type:              "header",
		TotalTargets:      len(targets),
		UnresolvedTargets: unresolvedTargets,
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

	// Group targets into dispatch waves. Without --bottom-up that is a single
	// wave holding everyone — byte-for-byte the historical broadcast. With it,
	// targets are ordered deepest-mesh-depth first so a relay is never executed
	// while any agent beneath it is still running (see targetIDWavesByDepthDesc).
	waves := [][]string{targetIDs}
	if req.BottomUp {
		waves = s.targetIDWavesByDepthDesc(targets)
	}

	metricInflightSessions.WithLabelValues("exec").Inc()
	defer metricInflightSessions.WithLabelValues("exec").Dec()

	opStart := time.Now()
	outcome := "complete"
	totalAccounted := 0

	for wi, wave := range waves {
		// A single wave keeps the historical request ID exactly; multi-wave
		// runs suffix it so each wave's agents key their responses distinctly.
		waveReqID := requestID
		if len(waves) > 1 {
			waveReqID = fmt.Sprintf("%s-w%d", requestID, wi)
		}

		waveOutcome, accounted := s.dispatchExecWave(ctx, enc, flusher, waveReqID, wi, wave, &req, stdinBytes, scriptBytes, timeout)
		totalAccounted += accounted

		// A wave that didn't fully account (sign failure, hard timeout, or
		// client cancellation) breaks the bottom-up ordering guarantee — the
		// state of the agents beneath the next wave is unknown — so we stop
		// rather than act on a relay whose subtree may still be running.
		// Command failures (non-zero rc) are terminal responses and do NOT
		// stop the run.
		if waveOutcome != "complete" {
			outcome = waveOutcome
			if len(waves) > 1 {
				// Tell the operator the shallower waves were deliberately not
				// attempted, so a stopped bottom-up run never reads as done.
				enc.Encode(execMultiResult{
					Type:  "result",
					Error: fmt.Sprintf("bottom-up run stopped after wave %d (%s); shallower waves not attempted", wi, waveOutcome),
				})
				flusher.Flush()
			}
			break
		}
	}

	dur := time.Since(opStart).Seconds()
	missing := len(targetIDs) - totalAccounted
	if outcome == "complete" && missing > 0 {
		outcome = "incomplete"
	}
	metricBroadcastTotal.WithLabelValues("exec", outcome).Inc()
	metricBroadcastDuration.WithLabelValues("exec").Observe(dur)
	if missing > 0 {
		metricBroadcastMissingTotal.WithLabelValues("exec").Add(float64(missing))
	}
}

// dispatchExecWave registers a per-wave broadcast session, dispatches the wave
// to the mesh, and streams its results. The session-map entry is removed via
// defer, so a panic while streaming cannot leak it (metricInflightSessions,
// deferred in the caller, then stays in sync). Returns the wave outcome
// ("complete", "hard_timeout", "canceled", or "incomplete" on a signing
// failure) and how many of the wave's agents were accounted.
func (s *Server) dispatchExecWave(ctx context.Context, enc *json.Encoder, flusher http.Flusher, waveReqID string, wi int, wave []string, req *execMultiRequest, stdin, script []byte, timeout int) (string, int) {
	// Hard timeout equals the command timeout plus a transport grace: the
	// agent kills the command at command-timeout, then needs a moment to flush
	// its ExecResponse back up the mesh. Without the grace we race the reply.
	bs := &execBroadcastSession{
		requestID:         waveReqID,
		results:           make(chan *pb.ExecResponse, len(wave)),
		startedAt:         time.Now(),
		timeout:           time.Duration(timeout)*time.Second + transportGrace,
		sessionAccounting: newSessionAccounting(wave),
	}

	execBroadcastSessionsMu.Lock()
	execBroadcastSessions[waveReqID] = bs
	execBroadcastSessionsMu.Unlock()
	defer func() {
		execBroadcastSessionsMu.Lock()
		delete(execBroadcastSessions, waveReqID)
		execBroadcastSessionsMu.Unlock()
	}()

	msg := buildExecMsg(waveReqID, wave, req, stdin, script, timeout)
	if err := s.signServerMessage(msg); err != nil {
		enc.Encode(execMultiResult{
			Type:    "result",
			Success: false,
			Error:   "sign failed: " + err.Error(),
		})
		flusher.Flush()
		return "incomplete", 0
	}

	sent, failedSubtrees := s.broadcastExecToZoneLeaders(msg, bs)
	s.log.Info("exec broadcast sent",
		"request_id", waveReqID,
		"wave", wi,
		"targets", len(wave),
		"zone_leaders", sent,
		"failed_subtrees", len(failedSubtrees),
	)

	return s.streamExecResults(ctx, enc, flusher, bs), bs.AccountedCount()
}

// buildExecMsg assembles the (unsigned) exec broadcast message for one wave of
// target agent IDs. Only the listed agents execute; the rest of the mesh
// relays it. Shared by the single-broadcast and bottom-up wave paths.
func buildExecMsg(requestID string, targetIDs []string, req *execMultiRequest, stdin, script []byte, timeout int) *pb.ServerMessage {
	return &pb.ServerMessage{
		Payload: &pb.ServerMessage_ExecRequest{
			ExecRequest: &pb.ExecRequest{
				RequestId:      requestID,
				TargetAgentIds: targetIDs,
				Command:        req.Command,
				Stdin:          stdin,
				Script:         script,
				ScriptName:     req.ScriptName,
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
}

// broadcastExecToZoneLeaders fans the exec request out to every connected
// zone-leader stream.  Same fanout-failure handling as the query
// dispatcher: a ZL whose buffer is full can't relay to its subtree, so we
// synthesize failures immediately.
func (s *Server) broadcastExecToZoneLeaders(msg *pb.ServerMessage, bs *execBroadcastSession) (sent int, failedSubtrees []string) {
	s.mu.RLock()
	for _, as := range s.streams {
		select {
		case as.send <- msg:
			sent++
		default:
			s.log.Warn("zone leader send buffer full during exec broadcast", "agent_id", as.agentID)
			failedSubtrees = append(failedSubtrees, as.agentID)
		}
	}
	s.mu.RUnlock()

	for _, zlID := range failedSubtrees {
		ids := s.topology.SubtreeIDs(zlID)
		for _, id := range ids {
			s.markGoneInExecSession(bs, id, "fanout to ZL failed")
		}
	}
	return sent, failedSubtrees
}

// streamExecResults streams results as agents respond and returns the
// outcome classification ("complete", "hard_timeout", or "canceled").
// No idle timeout — completion is driven by
// sessionAccounting.Remaining() reaching zero.  An unreachable agent is
// retired by the server-wide notifier when its stream closes (or
// PeerDisconnected propagates up, or the reaper times it out), which
// synthesizes a failure into bs.results and drains the pending set.  The
// hard timeout is a true backstop that shouldn't fire under normal
// conditions.
func (s *Server) streamExecResults(ctx context.Context, enc *json.Encoder, flusher http.Flusher, bs *execBroadcastSession) string {
	hardTimeout := time.NewTimer(bs.timeout)
	defer hardTimeout.Stop()
	progressTicker := time.NewTicker(5 * time.Second)
	defer progressTicker.Stop()

	emit := func(resp *pb.ExecResponse) {
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
	}

	for bs.Remaining() > 0 {
		select {
		case resp := <-bs.results:
			emit(resp)
		case <-progressTicker.C:
			enc.Encode(execMultiResult{
				Type:         "progress",
				Received:     bs.AccountedCount(),
				TotalTargets: bs.Total(),
			})
			flusher.Flush()
		case <-hardTimeout.C:
			s.log.Warn("exec broadcast hard-timeout fired",
				"request_id", bs.requestID,
				"accounted", bs.AccountedCount(),
				"targets", bs.Total(),
				"still_pending", bs.Remaining(),
			)
			return "hard_timeout"
		case <-ctx.Done():
			return "canceled"
		}
	}

	// Clean exit drain — see drainResults comment.  ClaimAgent decrements
	// Remaining BEFORE the result is consumed from bs.results, so a burst
	// of concurrent ClaimAgent calls can race Remaining() to zero while
	// real results still sit in the channel waiting to be encoded.
	// Without this drain those late items get GC'd and the CLI sees
	// fewer encodes than targets accounted (the "1006/1248 in 0.5s" bug).
	for {
		select {
		case resp := <-bs.results:
			emit(resp)
		default:
			return "complete"
		}
	}
}

// resolveFieldTargets runs a SELECT hostname query to find which agents
// match field-based WHERE conditions (e.g., os_info.os = 'linux').
// Returns the list of matching agent IDs and how many targets failed to
// respond — callers must propagate `missing` so the eventual exec result
// doesn't claim "N/N complete" when the underlying field resolution lost
// part of the fleet to idle/hard timeout.
func (s *Server) resolveFieldTargets(ctx context.Context, queryStr string, timeout int) (ids []string, missing int, err error) {
	body, _ := json.Marshal(map[string]any{
		"query":   queryStr,
		"timeout": timeout,
	})

	// Use the internal query handler by creating a fake HTTP request.
	// This is a bit of a hack but avoids duplicating the query dispatch logic.
	req, _ := http.NewRequestWithContext(ctx, "POST", "/api/v1/query", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := &responseRecorder{headers: make(http.Header), body: &bytes.Buffer{}}
	s.handleQuery(rec, req)

	if rec.code != http.StatusOK {
		return nil, 0, fmt.Errorf("query failed: HTTP %d", rec.code)
	}

	var result struct {
		Status  string `json:"status"`
		Missing int    `json:"missing"`
		Results []struct {
			AgentID string `json:"agent_id"`
			Success bool   `json:"success"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.body.Bytes(), &result); err != nil {
		return nil, 0, err
	}

	for _, r := range result.Results {
		if r.Success {
			ids = append(ids, r.AgentID)
		}
	}
	return ids, result.Missing, nil
}

// responseRecorder captures an HTTP response for internal use.
type responseRecorder struct {
	code    int
	headers http.Header
	body    *bytes.Buffer
}

func (r *responseRecorder) Header() http.Header         { return r.headers }
func (r *responseRecorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *responseRecorder) WriteHeader(code int)        { r.code = code }

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
