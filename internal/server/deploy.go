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

func (s *Server) handleDeployResponse(origin string, resp *pb.DeployResponse) {
	deploySessionsMu.RLock()
	ds, ok := deploySessions[resp.RequestId]
	deploySessionsMu.RUnlock()

	if ok {
		// Origin before accounting, for the same reason as exec: a forged
		// response that reaches ClaimAgent costs the victim its slot.
		if !s.originAllows(origin, resp.AgentId, "deploy_response") {
			return
		}
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
	// BottomUp installs deepest-mesh-depth first, one wave per depth, so a
	// relay is never updated while an agent beneath it is still installing.
	BottomUp bool `json:"bottom_up"`
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

	targets, unresolvedTargets, err := s.resolveBroadcastTargets(ctx, req.Query, parsed, timeout)
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

	header := map[string]any{
		"type":          "header",
		"total_targets": len(targets),
	}
	// Only present when the field-resolution pass lost part of the fleet, so
	// a fully-resolved deploy keeps the header it has always had. Mirrors the
	// exec header's unresolved_targets.
	if unresolvedTargets > 0 {
		header["unresolved_targets"] = unresolvedTargets
	}
	enc.Encode(header)
	flusher.Flush()

	if len(targets) == 0 {
		return
	}

	targetIDs := make([]string, len(targets))
	for i, a := range targets {
		targetIDs[i] = a.ID
	}

	requestID := fmt.Sprintf("deploy-%d", time.Now().UnixNano())

	// Group targets into dispatch waves. Without --bottom-up that is a single
	// wave holding everyone — the historical broadcast. With it, targets are
	// ordered deepest-mesh-depth first so a relay is never updated while any
	// agent beneath it is still installing (see targetIDWavesByDepthDesc).
	waves := [][]string{targetIDs}
	if req.BottomUp {
		waves = s.targetIDWavesByDepthDesc(targets)
	}

	metricInflightSessions.WithLabelValues("deploy").Inc()
	defer metricInflightSessions.WithLabelValues("deploy").Dec()

	opStart := time.Now()
	outcome := "complete"
	totalAccounted := 0

	for wi, wave := range waves {
		waveReqID := requestID
		if len(waves) > 1 {
			waveReqID = fmt.Sprintf("%s-w%d", requestID, wi)
		}

		waveOutcome, accounted := s.dispatchDeployWave(ctx, enc, flusher, waveReqID, wi, wave, &req, content, timeout)
		totalAccounted += accounted

		// A wave that didn't fully account (sign failure, hard timeout, or
		// cancellation) breaks the bottom-up ordering guarantee, so we stop
		// rather than update a relay whose subtree may still be installing. A
		// non-zero install rc is a terminal response and does NOT stop the run.
		if waveOutcome != "complete" {
			outcome = waveOutcome
			if len(waves) > 1 {
				enc.Encode(deployResultLine{
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
	metricBroadcastTotal.WithLabelValues("deploy", outcome).Inc()
	metricBroadcastDuration.WithLabelValues("deploy").Observe(dur)
	if missing > 0 {
		metricBroadcastMissingTotal.WithLabelValues("deploy").Add(float64(missing))
	}
}

// dispatchDeployWave registers a per-wave deploy session, dispatches the wave,
// and streams its results. The session-map entry is removed via defer so a
// panic while streaming cannot leak it. Returns the wave outcome ("complete",
// "hard_timeout", "canceled", or "incomplete" on a signing failure) and how
// many of the wave's agents were accounted.
func (s *Server) dispatchDeployWave(ctx context.Context, enc *json.Encoder, flusher http.Flusher, waveReqID string, wi int, wave []string, req *deployRequest, content []byte, timeout int) (string, int) {
	// Hard timeout = install timeout + transport grace, same shape as exec.
	ds := &deploySession{
		requestID:         waveReqID,
		results:           make(chan *pb.DeployResponse, len(wave)),
		startedAt:         time.Now(),
		timeout:           time.Duration(timeout)*time.Second + transportGrace,
		sessionAccounting: newSessionAccounting(wave),
	}

	deploySessionsMu.Lock()
	deploySessions[waveReqID] = ds
	deploySessionsMu.Unlock()
	defer func() {
		deploySessionsMu.Lock()
		delete(deploySessions, waveReqID)
		deploySessionsMu.Unlock()
	}()

	msg := buildDeployMsg(waveReqID, wave, req, content, timeout)
	if err := s.signServerMessage(msg); err != nil {
		enc.Encode(deployResultLine{
			Type:    "result",
			Success: false,
			Error:   "sign failed: " + err.Error(),
		})
		flusher.Flush()
		return "incomplete", 0
	}

	sent, failedSubtrees := s.broadcastDeployToZoneLeaders(msg, ds)
	s.log.Info("deploy broadcast sent",
		"request_id", waveReqID,
		"wave", wi,
		"targets", len(wave),
		"zone_leaders", sent,
		"failed_subtrees", len(failedSubtrees),
	)

	return s.streamDeployResults(ctx, enc, flusher, ds), ds.AccountedCount()
}

// buildDeployMsg assembles the (unsigned) deploy broadcast message for one wave
// of target agent IDs. Shared by the single-broadcast and bottom-up wave paths.
func buildDeployMsg(requestID string, targetIDs []string, req *deployRequest, content []byte, timeout int) *pb.ServerMessage {
	return &pb.ServerMessage{
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
