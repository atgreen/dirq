// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/query"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

type contextKey string

const tokenScopeKey contextKey = "tokenScope"

func (s *Server) setupHTTPRoutes() *http.ServeMux {
	mux := http.NewServeMux()

	// Rate limiter for broadcast endpoints: 10 requests/sec, burst of 20.
	broadcastRL := newRateLimiter(10, 20)

	// Read-only API routes (readonly or admin scope)
	mux.HandleFunc("POST /api/v1/query", s.authMiddleware(requireScope("readonly", s.rateLimitMiddleware(broadcastRL, s.handleQuery))))
	mux.HandleFunc("GET /api/v1/hosts", s.authMiddleware(requireScope("readonly", s.handleListHosts)))
	mux.HandleFunc("GET /api/v1/hosts/{id}", s.authMiddleware(requireScope("readonly", s.handleGetHost)))
	mux.HandleFunc("GET /api/v1/hosts/{id}/facts", s.authMiddleware(requireScope("readonly", s.handleGetHostFacts)))
	mux.HandleFunc("GET /api/v1/queries", s.authMiddleware(requireScope("readonly", s.handleListQueries)))
	mux.HandleFunc("GET /api/v1/exec_log", s.authMiddleware(requireScope("readonly", s.handleListExecLogs)))
	mux.HandleFunc("GET /api/v1/inventory", s.authMiddleware(requireScope("readonly", s.handleInventory)))

	// Write API routes (admin scope only)
	mux.HandleFunc("PUT /api/v1/hosts/{id}/tags", s.authMiddleware(requireScope("admin", s.handleSetTags)))
	mux.HandleFunc("PATCH /api/v1/hosts/{id}/tags", s.authMiddleware(requireScope("admin", s.handleMergeTags)))
	mux.HandleFunc("DELETE /api/v1/hosts/{id}/tags/{key}", s.authMiddleware(requireScope("admin", s.handleDeleteTag)))
	mux.HandleFunc("POST /api/v1/tokens", s.authMiddleware(requireScope("admin", s.handleCreateToken)))
	mux.HandleFunc("GET /api/v1/tokens", s.authMiddleware(requireScope("admin", s.handleListTokens)))
	mux.HandleFunc("DELETE /api/v1/tokens/{name}", s.authMiddleware(requireScope("admin", s.handleDeleteToken)))

	// Exec endpoints (admin scope only)
	mux.HandleFunc("POST /api/v1/exec", s.authMiddleware(requireScope("admin", s.rateLimitMiddleware(broadcastRL, s.handleExecCommand))))
	mux.HandleFunc("POST /api/v1/exec_multi", s.authMiddleware(requireScope("admin", s.rateLimitMiddleware(broadcastRL, s.handleExecMulti))))
	mux.HandleFunc("POST /api/v1/put_file", s.authMiddleware(requireScope("admin", s.handlePutFile)))
	mux.HandleFunc("POST /api/v1/fetch_file", s.authMiddleware(requireScope("admin", s.handleFetchFile)))
	mux.HandleFunc("POST /api/v1/deploy", s.authMiddleware(requireScope("admin", s.handleBroadcastDeploy)))

	// Status endpoint (authenticated, readonly)
	mux.HandleFunc("GET /api/v1/status", s.authMiddleware(requireScope("readonly", s.handleStatus)))

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
		if s.cfg.AuthDisabled {
			next(w, r)
			return
		}

		token := r.Header.Get("Authorization")
		if token == "" {
			httpError(w, http.StatusUnauthorized, "authentication required — provide an API token via Authorization: Bearer <token> header")
			return
		}

		if len(token) > 7 && token[:7] == "Bearer " {
			token = token[7:]
		}

		t, err := s.db.ValidateToken(r.Context(), token)
		if err != nil {
			httpError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		ctx := context.WithValue(r.Context(), tokenScopeKey, t.Scope)
		next(w, r.WithContext(ctx))
	}
}

func requireScope(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := r.Context().Value(tokenScopeKey).(string)
		if !ok {
			// No scope in context — auth was disabled, allow through.
			next(w, r)
			return
		}
		if s != "admin" && s != scope {
			httpError(w, http.StatusForbidden, "this endpoint requires "+scope+" or admin scope")
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

	// Determine target agents (online only — offline agents can't respond).
	ctx := r.Context()
	online := true
	allAgents, err := s.db.ListAgents(ctx, db.ListAgentsFilter{Online: &online})
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to list agents: "+err.Error())
		return
	}

	// Pre-filter by tag.* conditions in WHERE (avoids dispatching to irrelevant agents).
	agents := allAgents
	if query.HasTagConditions(parsed.Where) {
		agents = make([]db.Agent, 0, len(allAgents))
		for _, a := range allAgents {
			if query.MatchesAgentTags(parsed.Where, a.Tags) {
				agents = append(agents, a)
			}
		}
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

	// Build target agent ID list.
	targetIDs := make([]string, len(agents))
	for i, a := range agents {
		targetIDs[i] = a.ID
	}

	qr := &pb.QueryRequest{
		QueryId:        fmt.Sprintf("q-%d", time.Now().UnixNano()),
		RawQuery:       req.Query,
		Modules:        modules,
		Filters:        pbFilters,
		TimeoutSeconds: int32(timeout),
		TargetAgentIds: targetIDs,
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

	// Project fields: flatten module data and expand array modules into rows.
	// SELECT * skips projection (returns raw module data as before).
	isSelectStar := len(parsed.Select) == 1 && parsed.Select[0].Star
	if !isSelectStar && parsed.GroupBy == nil && !parsed.HasAggregates() {
		fields := make([]string, 0, len(parsed.Select))
		for _, s := range parsed.Select {
			if s.Field != "" {
				fields = append(fields, s.Field)
			}
		}
		if len(fields) > 0 {
			out = projectResults(out, fields)
		}
	}

	// Apply server-side aggregation if the query has GROUP BY or bare aggregates.
	aggregated := out
	if parsed.GroupBy != nil || parsed.HasAggregates() {
		rows := make([]query.Row, len(out))
		for i, r := range out {
			row := query.Row{}
			row["hostname"] = r.Hostname
			flattenInto(row, "", r.Data)
			rows[i] = row
		}
		aggRows, err := query.Aggregate(parsed, rows)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "aggregation failed: "+err.Error())
			return
		}
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

// resolveAgent looks up an agent by ID, falling back to hostname.
func (s *Server) resolveAgent(ctx context.Context, idOrHostname string) (db.Agent, error) {
	agent, err := s.db.GetAgent(ctx, idOrHostname)
	if err == nil {
		return agent, nil
	}
	return s.db.GetAgentByHostname(ctx, idOrHostname)
}

func (s *Server) handleGetHost(w http.ResponseWriter, r *http.Request) {
	agent, err := s.resolveAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "host not found")
		return
	}
	jsonResponse(w, http.StatusOK, agent)
}

func (s *Server) handleGetHostFacts(w http.ResponseWriter, r *http.Request) {
	agent, err := s.resolveAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "host not found")
		return
	}
	facts, err := s.db.GetFacts(r.Context(), agent.ID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, facts)
}

// ─────────────────────────────────────────────────────────
// Tag management
// ─────────────────────────────────────────────────────────

// PUT /api/v1/hosts/{id}/tags — replace all tags
func (s *Server) handleSetTags(w http.ResponseWriter, r *http.Request) {
	agent, err := s.resolveAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "host not found")
		return
	}
	var tags map[string]string
	if err := json.NewDecoder(r.Body).Decode(&tags); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: expected {\"key\": \"value\", ...}")
		return
	}
	if err := s.db.UpdateAgentTags(r.Context(), agent.ID, tags); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	agent, _ = s.db.GetAgent(r.Context(), agent.ID)
	jsonResponse(w, http.StatusOK, agent)
}

// PATCH /api/v1/hosts/{id}/tags — merge tags (add/update without removing existing)
func (s *Server) handleMergeTags(w http.ResponseWriter, r *http.Request) {
	agent, err := s.resolveAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "host not found")
		return
	}
	var newTags map[string]string
	if err := json.NewDecoder(r.Body).Decode(&newTags); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: expected {\"key\": \"value\", ...}")
		return
	}
	for k, v := range newTags {
		agent.Tags[k] = v
	}
	if err := s.db.UpdateAgentTags(r.Context(), agent.ID, agent.Tags); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	agent, _ = s.db.GetAgent(r.Context(), agent.ID)
	jsonResponse(w, http.StatusOK, agent)
}

// DELETE /api/v1/hosts/{id}/tags/{key} — remove a single tag
func (s *Server) handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	agent, err := s.resolveAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "host not found")
		return
	}
	key := r.PathValue("key")
	delete(agent.Tags, key)
	if err := s.db.UpdateAgentTags(r.Context(), agent.ID, agent.Tags); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

	// Build Ansible dynamic inventory with nested group hierarchy.
	//
	// Group structure:
	//   all
	//   ├── os_linux          (hosts with os=linux)
	//   ├── os_windows        (hosts with os=windows)
	//   ├── arch_amd64        (hosts with arch=amd64)
	//   ├── arch_arm64        (hosts with arch=arm64)
	//   ├── exec_enabled      (hosts with exec_enabled=true)
	//   ├── tag_env           (parent group for all env=* tags)
	//   │   ├── tag_env_prod
	//   │   └── tag_env_staging
	//   ├── tag_dc
	//   │   ├── tag_dc_us_east
	//   │   └── tag_dc_eu_west
	//   └── tag_role
	//       ├── tag_role_webserver
	//       └── tag_role_database
	//
	// Hosts are targetable as:
	//   hosts: all                 (every online host)
	//   hosts: os_linux            (all linux hosts)
	//   hosts: tag_env_prod        (hosts tagged env=prod)
	//   hosts: tag_role_webserver  (hosts tagged role=webserver)
	//   hosts: exec_enabled        (hosts that accept remote exec)
	//   hosts: fedora              (specific host by name)

	hostvars := map[string]any{}

	// Bulk-load all facts in a single query instead of N+1 (#6).
	allFacts, _ := s.db.GetAllFacts(ctx)
	factsByAgent := map[string][]db.Fact{}
	for _, f := range allFacts {
		factsByAgent[f.AgentID] = append(factsByAgent[f.AgentID], f)
	}

	// groups maps group name -> list of hostnames.
	groups := map[string][]string{}
	// parentGroups maps parent group name -> list of child group names.
	parentGroups := map[string][]string{}

	for _, agent := range agents {
		if !agent.Online {
			continue
		}

		hostname := agent.Hostname

		// Collect host vars.
		facts := factsByAgent[agent.ID]
		hostFacts := map[string]any{
			"dirq_agent_id":      agent.ID,
			"dirq_os":            agent.OS,
			"dirq_os_version":    agent.OSVersion,
			"dirq_arch":          agent.Arch,
			"dirq_agent_version": agent.AgentVersion,
			"dirq_exec_enabled":  agent.ExecEnabled,
			"dirq_online":        agent.Online,
			"dirq_last_seen":     agent.LastSeenAt.Format(time.RFC3339),
			"dirq_role":          agent.Role,
		}
		for _, f := range facts {
			hostFacts["dirq_"+f.Module] = f.Data
		}
		for k, v := range agent.Tags {
			hostFacts["dirq_tag_"+k] = v
		}
		hostvars[hostname] = hostFacts

		// Group by OS: os_linux, os_windows
		osGroup := "os_" + agent.OS
		groups[osGroup] = append(groups[osGroup], hostname)

		// Group by arch: arch_amd64, arch_arm64
		archGroup := "arch_" + agent.Arch
		groups[archGroup] = append(groups[archGroup], hostname)

		// Group by exec capability.
		if agent.ExecEnabled {
			groups["exec_enabled"] = append(groups["exec_enabled"], hostname)
		}

		// Group by tags with hierarchy.
		// Tag env=prod creates:
		//   - group "tag_env_prod" containing the host
		//   - parent group "tag_env" containing child group "tag_env_prod"
		for k, v := range agent.Tags {
			childGroup := "tag_" + sanitizeGroupName(k) + "_" + sanitizeGroupName(v)
			parentGroup := "tag_" + sanitizeGroupName(k)

			groups[childGroup] = append(groups[childGroup], hostname)

			// Track parent-child relationship (deduplicated later).
			found := false
			for _, existing := range parentGroups[parentGroup] {
				if existing == childGroup {
					found = true
					break
				}
			}
			if !found {
				parentGroups[parentGroup] = append(parentGroups[parentGroup], childGroup)
			}
		}
	}

	// Build the inventory JSON.
	inventory := map[string]any{
		"_meta": map[string]any{
			"hostvars": hostvars,
		},
	}

	// Add leaf groups (groups with hosts).
	for name, hosts := range groups {
		inventory[name] = map[string]any{
			"hosts": hosts,
		}
	}

	// Add parent groups (groups containing child groups, not hosts directly).
	for parent, children := range parentGroups {
		entry, ok := inventory[parent].(map[string]any)
		if !ok {
			entry = map[string]any{}
			inventory[parent] = entry
		}
		entry["children"] = children
	}

	jsonResponse(w, http.StatusOK, inventory)
}

// ─────────────────────────────────────────────────────────
// Status endpoint
// ─────────────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Database health.
	dbOK := true
	if err := s.db.Ping(ctx); err != nil {
		dbOK = false
	}

	// Agent counts.
	allAgents, _ := s.db.ListAgents(ctx, db.ListAgentsFilter{})
	online := true
	onlineAgents, _ := s.db.ListAgents(ctx, db.ListAgentsFilter{Online: &online})

	// Version distribution.
	versions := map[string]int{}
	for _, a := range onlineAgents {
		v := a.AgentVersion
		if v == "" {
			v = "unknown"
		}
		versions[v]++
	}

	// Topology stats.
	s.mu.RLock()
	zoneLeaders := len(s.streams)
	s.mu.RUnlock()

	// Find max tree depth and orphans (online agents with offline parents).
	maxDepth := 0
	orphans := 0
	parentOnline := map[string]bool{}
	for _, a := range allAgents {
		parentOnline[a.ID] = a.Online
	}
	for _, a := range onlineAgents {
		// Walk parent chain to compute depth.
		depth := 0
		seen := map[string]bool{}
		cur := a.ID
		for {
			found := false
			for _, p := range allAgents {
				if p.ID == cur && p.ParentID != nil && *p.ParentID != "" {
					if seen[cur] {
						break
					}
					seen[cur] = true
					depth++
					cur = *p.ParentID
					found = true
					break
				}
			}
			if !found {
				break
			}
		}
		if depth > maxDepth {
			maxDepth = depth
		}

		// Check for orphans.
		if a.ParentID != nil && *a.ParentID != "" {
			if isOnline, exists := parentOnline[*a.ParentID]; exists && !isOnline {
				orphans++
			}
		}
	}

	status := map[string]any{
		"database":         dbOK,
		"agents_total":     len(allAgents),
		"agents_online":    len(onlineAgents),
		"zone_leaders":     zoneLeaders,
		"max_tree_depth":   maxDepth,
		"orphaned_agents":  orphans,
		"agent_versions":   versions,
		"topology": map[string]any{
			"max_zone_leaders":     s.topoCfg.MaxZoneLeaders,
			"max_children_per_node": s.topoCfg.MaxChildrenPerNode,
		},
	}

	jsonResponse(w, http.StatusOK, status)
}

// sanitizeGroupName replaces characters that aren't valid in Ansible group
// names with underscores.
func sanitizeGroupName(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			b = append(b, c)
		} else {
			b = append(b, '_')
		}
	}
	return string(b)
}

// ─────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────

// projectResults takes raw agent results and projects them into flat rows
// with only the requested fields. Array modules (packages, services, disk,
// network) are expanded — one output row per array entry, carrying scalar
// fields along.
func projectResults(results []queryResult, fields []string) []queryResult {
	// Identify which array modules are referenced in the field list.
	arrayKeys := map[string]string{
		"packages": "packages",
		"services": "services",
		"disk":     "partitions",
		"network":  "interfaces",
	}

	arrayModulesUsed := map[string]string{} // module -> array key
	for _, f := range fields {
		parts := strings.SplitN(f, ".", 2)
		if len(parts) == 2 {
			if ak, ok := arrayKeys[parts[0]]; ok {
				arrayModulesUsed[parts[0]] = ak
			}
		}
	}

	var projected []queryResult
	for _, r := range results {
		if !r.Success || r.Data == nil {
			projected = append(projected, r)
			continue
		}

		// Flatten scalar modules into dotted keys.
		scalars := map[string]any{}
		scalars["hostname"] = r.Hostname
		for module, moduleData := range r.Data {
			if _, isArray := arrayKeys[module]; isArray {
				continue // handle separately
			}
			if nested, ok := moduleData.(map[string]any); ok {
				for k, v := range nested {
					scalars[module+"."+k] = v
				}
			}
		}

		// If no array modules are referenced, emit one row with scalar fields.
		if len(arrayModulesUsed) == 0 {
			row := projectRow(scalars, fields)
			projected = append(projected, queryResult{
				AgentID:     r.AgentID,
				Hostname:    r.Hostname,
				Success:     true,
				Data:        row,
				CollectedAt: r.CollectedAt,
			})
			continue
		}

		// Expand array modules into rows.
		// For simplicity, expand one array module at a time (the common case
		// is one array module per query, e.g. packages.name).
		for module, arrayKey := range arrayModulesUsed {
			moduleData, ok := r.Data[module]
			if !ok {
				continue
			}
			md, ok := moduleData.(map[string]any)
			if !ok {
				continue
			}
			arr, ok := md[arrayKey]
			if !ok {
				continue
			}
			items, ok := arr.([]any)
			if !ok {
				continue
			}

			for _, item := range items {
				entry, ok := item.(map[string]any)
				if !ok {
					continue
				}
				// Merge scalar fields with this array entry's fields.
				combined := map[string]any{}
				for k, v := range scalars {
					combined[k] = v
				}
				for k, v := range entry {
					combined[module+"."+k] = v
				}
				row := projectRow(combined, fields)
				projected = append(projected, queryResult{
					AgentID:     r.AgentID,
					Hostname:    r.Hostname,
					Success:     true,
					Data:        row,
					CollectedAt: r.CollectedAt,
				})
			}
		}
	}
	return projected
}

// projectRow picks only the requested fields from a flat key-value map.
func projectRow(src map[string]any, fields []string) map[string]any {
	row := make(map[string]any, len(fields))
	for _, f := range fields {
		if v, ok := src[f]; ok {
			row[f] = v
		}
	}
	return row
}

// flattenInto recursively flattens nested maps into dotted keys.
// e.g. {"os_info": {"os": "linux"}} becomes {"os_info.os": "linux"}.
func flattenInto(dst map[string]any, prefix string, src map[string]any) {
	for k, v := range src {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if nested, ok := v.(map[string]any); ok {
			flattenInto(dst, key, nested)
		} else {
			dst[key] = v
		}
	}
}

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
