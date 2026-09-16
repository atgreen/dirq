// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
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

// deploySession tracks an in-flight broadcast deploy.  Embeds the
// shared first-terminal-wins accounting so real DeployResponses and
// synthetic disconnect failures dedupe at the same gate as exec/query.
type deploySession struct {
	requestID string
	results   chan *pb.DeployResponse
	startedAt time.Time
	timeout   time.Duration
	*sessionAccounting
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
		// First-terminal-wins gate.
		if ds.ClaimAgent(resp.AgentId) {
			select {
			case ds.results <- resp:
			default:
				s.log.Warn("deploy result channel full", "request_id", resp.RequestId)
			}
		}
	}
}

// ─────────────────────────────────────────────────────────
// REST API: broadcast deploy
// ─────────────────────────────────────────────────────────

type deployRequest struct {
	Query          string `json:"query"`
	DestPath       string `json:"dest_path"`
	Content        string `json:"content"` // base64-encoded package binary
	Mode           int    `json:"mode"`
	InstallCommand string `json:"install_command"`
	Become         bool   `json:"become"`
	BecomeUser     string `json:"become_user"`
	Timeout        int    `json:"timeout"`
	// AAP attribution. Used for the server-side aap_user binding check. The
	// DeployRequest proto does not yet carry these fields to the agent, so an
	// agent-side deploy policy cannot see aap_user — the server binding is the
	// authoritative attribution check for deploy until the proto is extended.
	AAPJobID       string `json:"aap_job_id"`
	AAPJobTemplate string `json:"aap_job_template"`
	AAPUser        string `json:"aap_user"`
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
	req, content, timeout, ok := s.decodeDeployRequest(w, r)
	if !ok {
		return
	}

	ctx := r.Context()
	parsed, err := query.Parse(req.Query)
	if err != nil {
		httpError(w, http.StatusBadRequest, "query parse error: "+err.Error())
		return
	}

	targets, err := s.resolveDeployTargets(ctx, parsed)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)

	enc.Encode(map[string]any{
		"type":          "header",
		"total_targets": len(targets),
	})
	flusher.Flush()

	if len(targets) == 0 {
		return
	}

	targetIDs := make([]string, len(targets))
	for i, a := range targets {
		targetIDs[i] = a.ID
	}

	requestID := fmt.Sprintf("deploy-%d", time.Now().UnixNano())

	// Hard timeout = install timeout + transport grace, same shape as exec.
	ds := &deploySession{
		requestID:         requestID,
		results:           make(chan *pb.DeployResponse, len(targets)),
		startedAt:         time.Now(),
		timeout:           time.Duration(timeout)*time.Second + transportGrace,
		sessionAccounting: newSessionAccounting(targetIDs),
	}

	deploySessionsMu.Lock()
	deploySessions[requestID] = ds
	deploySessionsMu.Unlock()

	outcome := "complete"
	metricInflightSessions.WithLabelValues("deploy").Inc()
	defer func() {
		metricInflightSessions.WithLabelValues("deploy").Dec()
		dur := time.Since(ds.startedAt).Seconds()
		missing := ds.Total() - ds.AccountedCount()
		if outcome == "complete" && missing > 0 {
			outcome = "incomplete"
		}
		metricBroadcastTotal.WithLabelValues("deploy", outcome).Inc()
		metricBroadcastDuration.WithLabelValues("deploy").Observe(dur)
		if missing > 0 {
			metricBroadcastMissingTotal.WithLabelValues("deploy").Add(float64(missing))
		}
	}()

	defer func() {
		deploySessionsMu.Lock()
		delete(deploySessions, requestID)
		deploySessionsMu.Unlock()
	}()

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

	sent, failedSubtrees := s.broadcastDeployToZoneLeaders(msg, ds)

	s.log.Info("deploy broadcast sent",
		"request_id", requestID,
		"targets", len(targetIDs),
		"zone_leaders", sent,
		"failed_subtrees", len(failedSubtrees),
	)

	outcome = s.streamDeployResults(ctx, enc, flusher, ds)
}

// decodeDeployRequest decodes and validates a broadcast-deploy request,
// returning the decoded package content and the effective timeout in
// seconds.  On any failure it writes the HTTP error and returns ok=false.
//
// The check order is load-bearing: the AAP binding check sits between the
// install-command and dest-path checks, so a request that is both
// unauthorized and missing dest_path answers 403, not 400.  Reordering
// these changes the status callers see.
func (s *Server) decodeDeployRequest(w http.ResponseWriter, r *http.Request) (req deployRequest, content []byte, timeout int, ok bool) {
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return req, nil, 0, false
	}
	if req.Content == "" {
		httpError(w, http.StatusBadRequest, "content is required")
		return req, nil, 0, false
	}
	if req.InstallCommand == "" {
		httpError(w, http.StatusBadRequest, "install_command is required")
		return req, nil, 0, false
	}
	if err := s.bindAAP(r, req.AAPUser); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
		return req, nil, 0, false
	}
	if req.DestPath == "" {
		httpError(w, http.StatusBadRequest, "dest_path is required")
		return req, nil, 0, false
	}
	if req.Query == "" {
		httpError(w, http.StatusBadRequest, "query is required")
		return req, nil, 0, false
	}

	content, err := decodeBase64(req.Content)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid base64 content: "+err.Error())
		return req, nil, 0, false
	}

	timeout = req.Timeout
	if timeout == 0 {
		timeout = 300
	}
	return req, content, timeout, true
}

// resolveDeployTargets returns the online, exec-enabled agents matching the
// query.
//
// NOTE: this honours tag conditions only.  A query filtering on hostname or
// on a fact field contains no tag condition, so the filter is skipped
// entirely and every online exec-enabled agent is targeted — see dirq-8cp.
// The exec path (resolveExecTargets) handles both; deploy has not been
// brought in line with it yet.
func (s *Server) resolveDeployTargets(ctx context.Context, parsed *query.Query) ([]db.Agent, error) {
	online := true
	allAgents, err := s.db.ListAgents(ctx, db.ListAgentsFilter{Online: &online})
	if err != nil {
		return nil, fmt.Errorf("failed to list agents: %w", err)
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

	var targets []db.Agent
	for _, a := range agents {
		if a.ExecEnabled {
			targets = append(targets, a)
		}
	}
	return targets, nil
}

// broadcastDeployToZoneLeaders fans the deploy request out to every
// connected zone-leader stream.  Same fanout-failure handling as the
// query/exec dispatchers: a ZL whose buffer is full can't relay to its
// subtree, so we synthesize failures for that subtree immediately rather
// than wait for responses that can never arrive.
func (s *Server) broadcastDeployToZoneLeaders(msg *pb.ServerMessage, ds *deploySession) (sent int, failedSubtrees []string) {
	s.mu.RLock()
	for _, as := range s.streams {
		select {
		case as.send <- msg:
			sent++
		default:
			s.log.Warn("zone leader send buffer full during deploy", "agent_id", as.agentID)
			failedSubtrees = append(failedSubtrees, as.agentID)
		}
	}
	s.mu.RUnlock()

	for _, zlID := range failedSubtrees {
		for _, id := range s.topology.SubtreeIDs(zlID) {
			s.markGoneInDeploySession(ds, id, "fanout to ZL failed")
		}
	}
	return sent, failedSubtrees
}

// streamDeployResults streams results as agents respond and returns the
// outcome classification ("complete", "hard_timeout", or "canceled").
// Completion is driven by sessionAccounting.Remaining() reaching zero
// rather than an idle timeout — an unreachable agent is retired by the
// server-wide notifier, which synthesizes a failure into ds.results.  The
// hard timeout is a true backstop that shouldn't fire under normal
// conditions.
func (s *Server) streamDeployResults(ctx context.Context, enc *json.Encoder, flusher http.Flusher, ds *deploySession) string {
	hardTimeout := time.NewTimer(ds.timeout)
	defer hardTimeout.Stop()

	emit := func(resp *pb.DeployResponse) {
		enc.Encode(deployResultLine{
			Type:     "result",
			AgentID:  resp.AgentId,
			Hostname: resp.Hostname,
			Success:  resp.Success,
			Error:    resp.Error,
			Phase:    resp.Phase,
			RC:       int(resp.Rc),
			Stdout:   encodeBase64(resp.Stdout),
			Stderr:   encodeBase64(resp.Stderr),
		})
		flusher.Flush()
	}

	for ds.Remaining() > 0 {
		select {
		case resp := <-ds.results:
			emit(resp)
		case <-hardTimeout.C:
			s.log.Warn("deploy broadcast hard-timeout fired",
				"request_id", ds.requestID,
				"accounted", ds.AccountedCount(),
				"targets", ds.Total(),
				"still_pending", ds.Remaining(),
			)
			return "hard_timeout"
		case <-ctx.Done():
			return "canceled"
		}
	}

	// Clean-exit drain — see streamExecResults for the full rationale.
	// ClaimAgent decrements Remaining BEFORE the result is consumed from
	// ds.results, so under burst arrivals Remaining() can hit zero while
	// real results still sit in the channel; the drain catches them before
	// the dispatcher returns.
	for {
		select {
		case resp := <-ds.results:
			emit(resp)
		default:
			return "complete"
		}
	}
}
