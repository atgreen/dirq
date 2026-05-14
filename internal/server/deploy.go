// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
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
// Deploy session tracking
// ─────────────────────────────────────────────────────────

type deploySession struct {
	requestID string
	results   chan *pb.DeployResponse
	startedAt time.Time
	timeout   time.Duration
}

var (
	deploySessions   = make(map[string]*deploySession)
	deploySessionsMu sync.RWMutex
)

func (s *Server) handleDeployResponse(resp *pb.DeployResponse) {
	deploySessionsMu.RLock()
	ds, ok := deploySessions[resp.RequestId]
	deploySessionsMu.RUnlock()

	if ok {
		select {
		case ds.results <- resp:
		default:
			s.log.Warn("deploy result channel full", "request_id", resp.RequestId)
		}
	}
}

// ─────────────────────────────────────────────────────────
// REST API: broadcast deploy
// ─────────────────────────────────────────────────────────

type deployRequest struct {
	Query          string `json:"query"`
	DestPath       string `json:"dest_path"`
	Content        string `json:"content"`         // base64-encoded package binary
	Mode           int    `json:"mode"`
	InstallCommand string `json:"install_command"`
	Become         bool   `json:"become"`
	BecomeUser     string `json:"become_user"`
	Timeout        int    `json:"timeout"`
}

type deployResultLine struct {
	Type     string `json:"type"`
	AgentID  string `json:"agent_id,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Success  bool   `json:"success"`
	Error    string `json:"error,omitempty"`
	Phase    string `json:"phase,omitempty"`
	RC       int    `json:"rc,omitempty"`
	Stdout   string `json:"stdout,omitempty"` // base64-encoded
	Stderr   string `json:"stderr,omitempty"` // base64-encoded
}

func (s *Server) handleBroadcastDeploy(w http.ResponseWriter, r *http.Request) {
	var req deployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.Content == "" {
		httpError(w, http.StatusBadRequest, "content is required")
		return
	}
	if req.InstallCommand == "" {
		httpError(w, http.StatusBadRequest, "install_command is required")
		return
	}
	if req.DestPath == "" {
		httpError(w, http.StatusBadRequest, "dest_path is required")
		return
	}
	if req.Query == "" {
		httpError(w, http.StatusBadRequest, "query is required")
		return
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = 300
	}

	// Decode base64 content.
	content, err := decodeBase64(req.Content)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid base64 content: "+err.Error())
		return
	}

	// Resolve target agents.
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

	agents := allAgents
	if query.HasTagConditions(parsed.Where) {
		agents = make([]db.Agent, 0, len(allAgents))
		for _, a := range allAgents {
			if query.MatchesAgentTags(parsed.Where, a.Tags) {
				agents = append(agents, a)
			}
		}
	}

	// Filter to exec-enabled agents.
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
	enc.Encode(map[string]any{
		"type":          "header",
		"total_targets": len(targets),
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

	requestID := fmt.Sprintf("deploy-%d", time.Now().UnixNano())

	// Create deploy session to collect responses.
	ds := &deploySession{
		requestID: requestID,
		results:   make(chan *pb.DeployResponse, len(targets)),
		startedAt: time.Now(),
		timeout:   time.Duration(timeout) * time.Second,
	}

	deploySessionsMu.Lock()
	deploySessions[requestID] = ds
	deploySessionsMu.Unlock()

	defer func() {
		deploySessionsMu.Lock()
		delete(deploySessions, requestID)
		deploySessionsMu.Unlock()
	}()

	// Build the broadcast message.
	msg := &pb.ServerMessage{
		Payload: &pb.ServerMessage_DeployRequest{
			DeployRequest: &pb.DeployRequest{
				RequestId:      requestID,
				TargetAgentIds: targetIDs,
				DestPath:       req.DestPath,
				Content:        content,
				Mode:           int32(req.Mode),
				InstallCommand: req.InstallCommand,
				Become:         req.Become,
				BecomeUser:     req.BecomeUser,
				TimeoutSeconds: int32(timeout),
			},
		},
	}

	if err := s.signServerMessage(msg); err != nil {
		enc.Encode(deployResultLine{
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
			s.log.Warn("zone leader send buffer full during deploy", "agent_id", as.agentID)
		}
	}
	s.mu.RUnlock()

	s.log.Info("deploy broadcast sent",
		"request_id", requestID,
		"targets", len(targetIDs),
		"zone_leaders", sent,
	)

	// Stream results as agents respond.
	hardTimeout := time.NewTimer(ds.timeout)
	defer hardTimeout.Stop()
	idleTimeout := time.NewTimer(30 * time.Second)
	defer idleTimeout.Stop()

	received := 0
	for received < len(targets) {
		select {
		case resp := <-ds.results:
			received++
			out := deployResultLine{
				Type:     "result",
				AgentID:  resp.AgentId,
				Hostname: resp.Hostname,
				Success:  resp.Success,
				Error:    resp.Error,
				Phase:    resp.Phase,
				RC:       int(resp.Rc),
				Stdout:   encodeBase64(resp.Stdout),
				Stderr:   encodeBase64(resp.Stderr),
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
			s.log.Warn("deploy timed out", "request_id", requestID, "received", received, "targets", len(targets))
			return
		case <-ctx.Done():
			return
		}
	}
}
