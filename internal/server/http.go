package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/query"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

func (s *Server) setupHTTPRoutes() *http.ServeMux {
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("POST /api/v1/query", s.authMiddleware(s.handleQuery))
	mux.HandleFunc("GET /api/v1/hosts", s.authMiddleware(s.handleListHosts))
	mux.HandleFunc("GET /api/v1/hosts/{id}", s.authMiddleware(s.handleGetHost))
	mux.HandleFunc("GET /api/v1/hosts/{id}/facts", s.authMiddleware(s.handleGetHostFacts))
	mux.HandleFunc("GET /api/v1/queries", s.authMiddleware(s.handleListQueries))
	mux.HandleFunc("POST /api/v1/tokens", s.authMiddleware(s.handleCreateToken))
	mux.HandleFunc("GET /api/v1/tokens", s.authMiddleware(s.handleListTokens))
	mux.HandleFunc("DELETE /api/v1/tokens/{name}", s.authMiddleware(s.handleDeleteToken))

	// Phase 2: exec endpoints
	mux.HandleFunc("POST /api/v1/exec", s.authMiddleware(s.handleExecCommand))
	mux.HandleFunc("POST /api/v1/put_file", s.authMiddleware(s.handlePutFile))
	mux.HandleFunc("POST /api/v1/fetch_file", s.authMiddleware(s.handleFetchFile))
	mux.HandleFunc("GET /api/v1/exec_log", s.authMiddleware(s.handleListExecLogs))

	// Ansible inventory endpoint
	mux.HandleFunc("GET /api/v1/inventory", s.authMiddleware(s.handleInventory))

	// Health check (no auth)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	return mux
}

// ─────────────────────────────────────────────────────────
// Auth middleware
// ─────────────────────────────────────────────────────────

func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if token == "" {
			// In dev mode, allow unauthenticated access
			// TODO: make this configurable
			next(w, r)
			return
		}

		if len(token) > 7 && token[:7] == "Bearer " {
			token = token[7:]
		}

		_, err := s.db.ValidateToken(r.Context(), token)
		if err != nil {
			httpError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next(w, r)
	}
}

// ─────────────────────────────────────────────────────────
// Query endpoint
// ─────────────────────────────────────────────────────────

type queryRequest struct {
	Query   string `json:"query"`
	Timeout int    `json:"timeout"` // seconds
}

type queryResponse struct {
	QueryID      string        `json:"query_id"`
	Status       string        `json:"status"`
	TotalTargets int           `json:"total_targets"`
	Received     int           `json:"received"`
	Results      []queryResult `json:"results"`
}

type queryResult struct {
	AgentID     string         `json:"agent_id"`
	Hostname    string         `json:"hostname"`
	Success     bool           `json:"success"`
	Error       string         `json:"error,omitempty"`
	Data        map[string]any `json:"data,omitempty"`
	CollectedAt time.Time      `json:"collected_at"`
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.Query == "" {
		httpError(w, http.StatusBadRequest, "query is required")
		return
	}

	// Parse the DirQ query DSL.
	parsed, err := query.Parse(req.Query)
	if err != nil {
		httpError(w, http.StatusBadRequest, "query parse error: "+err.Error())
		return
	}

	// Determine target agents.
	ctx := r.Context()
	target := query.ExtractTarget(parsed)
	agentFilter := db.ListAgentsFilter{}
	if !target.All {
		agentFilter.Tag = target.Kind
		agentFilter.TagValue = target.Value
	}
	agents, err := s.db.ListAgents(ctx, agentFilter)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to list agents: "+err.Error())
		return
	}

	if len(agents) == 0 {
		jsonResponse(w, http.StatusOK, queryResponse{
			Status:       "completed",
			TotalTargets: 0,
			Results:      []queryResult{},
		})
		return
	}

	// Build QueryRequest proto.
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 60
	}

	modules := query.ExtractModules(parsed)
	pbFilters := query.ToFilterProtos(parsed)

	qr := &pb.QueryRequest{
		QueryId:        fmt.Sprintf("q-%d", time.Now().UnixNano()),
		RawQuery:       req.Query,
		Modules:        modules,
		Filters:        pbFilters,
		TimeoutSeconds: int32(timeout),
	}

	// Record query in DB.
	targetIDs := make([]string, len(agents))
	for i, a := range agents {
		targetIDs[i] = a.ID
	}

	dbQuery, err := s.db.CreateQuery(ctx, req.Query, "", len(targetIDs))
	if err != nil {
		s.log.Error("failed to record query", "error", err)
	}

	// Dispatch to agents.
	results, err := s.dispatchQuery(ctx, qr, targetIDs)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "query dispatch failed: "+err.Error())
		return
	}

	// Convert results.
	successCount := 0
	errorCount := 0
	out := make([]queryResult, len(results))
	for i, r := range results {
		var data map[string]any
		if r.Data != nil {
			data = r.Data.AsMap()
		}
		collectedAt := time.Now()
		if r.CollectedAt != nil {
			collectedAt = r.CollectedAt.AsTime()
		}
		out[i] = queryResult{
			AgentID:     r.AgentId,
			Hostname:    r.Hostname,
			Success:     r.Success,
			Error:       r.Error,
			Data:        data,
			CollectedAt: collectedAt,
		}
		if r.Success {
			successCount++
		} else {
			errorCount++
		}
	}

	// Apply server-side aggregation if the query has GROUP BY.
	aggregated := out
	if parsed.GroupBy != nil {
		rows := make([]query.Row, len(out))
		for i, r := range out {
			row := query.Row{}
			row["hostname"] = r.Hostname
			for k, v := range r.Data {
				row[k] = v
			}
			rows[i] = row
		}
		aggRows, _ := query.Aggregate(parsed, rows)
		aggregated = make([]queryResult, len(aggRows))
		for i, ar := range aggRows {
			aggregated[i] = queryResult{
				Success: true,
				Data:    ar.Values,
			}
		}
	}

	// Update query record.
	if dbQuery.ID != "" {
		timeoutCount := len(targetIDs) - len(results)
		s.db.UpdateQueryStatus(ctx, dbQuery.ID, "completed", successCount, errorCount, timeoutCount)
	}

	jsonResponse(w, http.StatusOK, queryResponse{
		QueryID:      qr.QueryId,
		Status:       "completed",
		TotalTargets: len(targetIDs),
		Received:     len(results),
		Results:      aggregated,
	})
}

// ─────────────────────────────────────────────────────────
// Host endpoints
// ─────────────────────────────────────────────────────────

func (s *Server) handleListHosts(w http.ResponseWriter, r *http.Request) {
	agents, err := s.db.ListAgents(r.Context(), db.ListAgentsFilter{})
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, agents)
}

func (s *Server) handleGetHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	agent, err := s.db.GetAgent(r.Context(), id)
	if err != nil {
		httpError(w, http.StatusNotFound, "host not found")
		return
	}
	jsonResponse(w, http.StatusOK, agent)
}

func (s *Server) handleGetHostFacts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	facts, err := s.db.GetFacts(r.Context(), id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, facts)
}

// ─────────────────────────────────────────────────────────
// Query history
// ─────────────────────────────────────────────────────────

func (s *Server) handleListQueries(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	queries, err := s.db.ListQueries(r.Context(), limit)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, queries)
}

// ─────────────────────────────────────────────────────────
// Token endpoints
// ─────────────────────────────────────────────────────────

type createTokenRequest struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req createTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Scope == "" {
		req.Scope = "admin"
	}

	plaintext, err := s.db.CreateToken(r.Context(), req.Name, req.Scope)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to create token: "+err.Error())
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]string{
		"name":  req.Name,
		"token": plaintext,
		"scope": req.Scope,
	})
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.db.ListTokens(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, tokens)
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.db.DeleteToken(r.Context(), name); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─────────────────────────────────────────────────────────
// Ansible inventory endpoint
// ─────────────────────────────────────────────────────────

func (s *Server) handleInventory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agents, err := s.db.ListAgents(ctx, db.ListAgentsFilter{})
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Build Ansible dynamic inventory format.
	inventory := map[string]any{
		"_meta": map[string]any{
			"hostvars": map[string]any{},
		},
	}

	allHosts := []string{}
	hostvars := inventory["_meta"].(map[string]any)["hostvars"].(map[string]any)
	groups := map[string][]string{}

	for _, agent := range agents {
		if !agent.Online {
			continue
		}

		allHosts = append(allHosts, agent.Hostname)

		// Get cached facts for this agent.
		facts, _ := s.db.GetFacts(ctx, agent.ID)
		hostFacts := map[string]any{
			"dirq_agent_id":      agent.ID,
			"dirq_os":            agent.OS,
			"dirq_os_version":    agent.OSVersion,
			"dirq_arch":          agent.Arch,
			"dirq_agent_version": agent.AgentVersion,
			"dirq_online":        agent.Online,
			"dirq_last_seen":     agent.LastSeenAt.Format(time.RFC3339),
		}

		for _, f := range facts {
			hostFacts["dirq_"+f.Module] = f.Data
		}

		// Tags become Ansible host vars too.
		for k, v := range agent.Tags {
			hostFacts["dirq_tag_"+k] = v
		}

		hostvars[agent.Hostname] = hostFacts

		// Group by OS.
		groups[agent.OS] = append(groups[agent.OS], agent.Hostname)

		// Group by tags.
		for k, v := range agent.Tags {
			groupName := k + "_" + v
			groups[groupName] = append(groups[groupName], agent.Hostname)
		}
	}

	inventory["all"] = map[string]any{
		"hosts": allHosts,
	}

	for name, hosts := range groups {
		inventory[name] = map[string]any{
			"hosts": hosts,
		}
	}

	jsonResponse(w, http.StatusOK, inventory)
}

// ─────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func jsonResponse(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}
