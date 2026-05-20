// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/atgreen/dirq/internal/db"
	"github.com/jackc/pgx/v5"
)

// Ensure mockDB implements db.DB at compile time.
var _ db.DB = (*mockDB)(nil)

// mockDB implements the DB interface for testing.
type mockDB struct {
	tokens     []mockToken
	agents     []db.Agent
	execLogs   []db.ExecLog
	queries    []db.Query
	facts      map[string][]db.Fact
	lastDelete string
}

type mockToken struct {
	plaintext string
	token     db.Token
}

func (m *mockDB) Ping(context.Context) error      { return nil }
func (m *mockDB) Kind() string                    { return "mock" }
func (m *mockDB) NewLeader(*slog.Logger) db.Leader { return mockLeader{} }

type mockLeader struct{}

func (mockLeader) IsLeader() bool          { return true }
func (mockLeader) Run(context.Context)      {}

func (m *mockDB) ValidateToken(_ context.Context, plaintext string) (db.Token, error) {
	for _, t := range m.tokens {
		if t.plaintext == plaintext {
			return t.token, nil
		}
	}
	return db.Token{}, pgx.ErrNoRows
}

func (m *mockDB) CreateToken(_ context.Context, name, scope string) (string, error) {
	return "mock-token-plaintext", nil
}

func (m *mockDB) ListTokens(_ context.Context) ([]db.Token, error) {
	var tokens []db.Token
	for _, t := range m.tokens {
		tokens = append(tokens, t.token)
	}
	return tokens, nil
}

func (m *mockDB) DeleteToken(_ context.Context, name string) error {
	m.lastDelete = name
	return nil
}

func (m *mockDB) GetAgent(_ context.Context, id string) (db.Agent, error) {
	for _, a := range m.agents {
		if a.ID == id {
			return a, nil
		}
	}
	return db.Agent{}, pgx.ErrNoRows
}

func (m *mockDB) ListAgents(_ context.Context, _ db.ListAgentsFilter) ([]db.Agent, error) {
	return m.agents, nil
}

func (m *mockDB) UpdateAgentTags(_ context.Context, id string, tags map[string]string) error {
	for i, a := range m.agents {
		if a.ID == id {
			m.agents[i].Tags = tags
			return nil
		}
	}
	return pgx.ErrNoRows
}

func (m *mockDB) GetFacts(_ context.Context, agentID string) ([]db.Fact, error) {
	return m.facts[agentID], nil
}

func (m *mockDB) GetAllFacts(_ context.Context) ([]db.Fact, error) {
	var all []db.Fact
	for _, facts := range m.facts {
		all = append(all, facts...)
	}
	return all, nil
}

func (m *mockDB) ListQueries(_ context.Context, _ int) ([]db.Query, error) {
	return m.queries, nil
}

func (m *mockDB) ListExecLogs(_ context.Context, _ int) ([]db.ExecLog, error) {
	return m.execLogs, nil
}

func (m *mockDB) ListExecLogsByAgent(_ context.Context, _ string, _ int) ([]db.ExecLog, error) {
	return m.execLogs, nil
}

func (m *mockDB) ListExecLogsByJob(_ context.Context, _ string) ([]db.ExecLog, error) {
	return m.execLogs, nil
}

// Unused by HTTP handler tests — stub out.
func (m *mockDB) RegisterAgent(context.Context, db.RegisterAgentParams) (db.Agent, error) {
	return db.Agent{}, nil
}
func (m *mockDB) UpdateAgentHeartbeat(context.Context, string) error  { return nil }
func (m *mockDB) SetAgentOffline(context.Context, string) error       { return nil }
func (m *mockDB) SetAgentRole(context.Context, string, string) error  { return nil }
func (m *mockDB) SetAgentParent(context.Context, string, string) error { return nil }
func (m *mockDB) MarkStaleAgentsOffline(context.Context, time.Duration) (int64, error) {
	return 0, nil
}
func (m *mockDB) TouchAgentTree(context.Context, string) error             { return nil }
func (m *mockDB) MarkAgentTreeOffline(context.Context, string) (int64, error) { return 0, nil }
func (m *mockDB) UpsertFact(context.Context, string, string, map[string]any) error { return nil }
func (m *mockDB) CreateQuery(_ context.Context, _ string, _ string, _ int) (db.Query, error) {
	return db.Query{}, nil
}
func (m *mockDB) UpdateQueryStatus(context.Context, string, string, int, int, int) error {
	return nil
}
func (m *mockDB) CreateExecLog(_ context.Context, _ db.ExecLog) (db.ExecLog, error) {
	return db.ExecLog{}, nil
}
func (m *mockDB) UpdateExecLog(context.Context, string, *int, bool, string, time.Time) error {
	return nil
}
func (m *mockDB) WithTopologyLock(_ context.Context, fn func() error) error { return fn() }
func (m *mockDB) FindZoneLeader(context.Context, string) (db.Agent, error) {
	return db.Agent{}, nil
}
func (m *mockDB) FindShallowestParentWithRoom(context.Context, int) (db.Agent, error) {
	return db.Agent{}, nil
}
func (m *mockDB) FindFallbackParents(context.Context, string, int, int) ([]db.Agent, error) {
	return nil, nil
}
func (m *mockDB) CountAgentsByRole(context.Context, string) (int, error)    { return 0, nil }
func (m *mockDB) CountOnlineZoneLeaders(context.Context) (int, error)       { return 0, nil }
func (m *mockDB) CountChildren(context.Context, string) (int, error)        { return 0, nil }
func (m *mockDB) FindRelaysWithChildren(context.Context) ([]db.Agent, error) { return nil, nil }
func (m *mockDB) FindImbalancedNodes(context.Context, int) (db.NodeLoad, db.NodeLoad, bool, error) {
	return db.NodeLoad{}, db.NodeLoad{}, false, nil
}
func (m *mockDB) FindChildOfParent(context.Context, string) (db.Agent, error) {
	return db.Agent{}, nil
}
func (m *mockDB) RegisterServerPeer(context.Context, string, string) error { return nil }
func (m *mockDB) Close()                                                   {}
func (m *mockDB) RunMigrations(context.Context) error                      { return nil }
func (m *mockDB) DeleteAgent(context.Context, string) error                { return nil }
func (m *mockDB) GetAgentByHostname(_ context.Context, hostname string) (db.Agent, error) {
	for _, a := range m.agents {
		if a.Hostname == hostname {
			return a, nil
		}
	}
	return db.Agent{}, pgx.ErrNoRows
}
func (m *mockDB) GetFactsByModule(context.Context, string) ([]db.Fact, error) { return nil, nil }
func (m *mockDB) GetFactTTL(context.Context, string) (int, error)            { return 900, nil }
func (m *mockDB) FindParentWithRoom(context.Context, string, int) (db.Agent, error) {
	return db.Agent{}, nil
}
func (m *mockDB) ListServerPeers(context.Context) ([]db.ServerPeer, error) { return nil, nil }
func (m *mockDB) RemoveServerPeer(context.Context, string) error           { return nil }

// ─────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────

func newTestServer(mock *mockDB, authDisabled bool) *Server {
	return &Server{
		cfg:          Config{AuthDisabled: authDisabled},
		db:           mock,
		log:          slog.Default(),
		streams:      make(map[string]*agentStream),
		execSessions: make(map[string]*execSession),
		reassigning:  make(map[string]time.Time),
	}
}

// ─────────────────────────────────────────────────────────
// Auth middleware tests
// ─────────────────────────────────────────────────────────

func TestAuthMiddleware_NoToken_AuthRequired(t *testing.T) {
	s := newTestServer(&mockDB{}, false)
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_NoToken_AuthDisabled(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	s := newTestServer(&mockDB{}, false)
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_ValidToken_SetsScope(t *testing.T) {
	mock := &mockDB{
		tokens: []mockToken{
			{plaintext: "good-token", token: db.Token{Scope: "readonly"}},
		},
	}
	s := newTestServer(mock, false)

	var gotScope string
	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		gotScope, _ = r.Context().Value(tokenScopeKey).(string)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotScope != "readonly" {
		t.Fatalf("expected scope 'readonly', got %q", gotScope)
	}
}

func TestAuthMiddleware_QueryParamTokenRejected(t *testing.T) {
	mock := &mockDB{
		tokens: []mockToken{
			{plaintext: "query-token", token: db.Token{Scope: "admin"}},
		},
	}
	s := newTestServer(mock, false)

	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Query param tokens are no longer accepted — must use Authorization header.
	req := httptest.NewRequest("GET", "/test?token=query-token", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// ─────────────────────────────────────────────────────────
// Scope enforcement tests
// ─────────────────────────────────────────────────────────

func TestRequireScope_AdminAllowed(t *testing.T) {
	handler := requireScope("readonly", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	ctx := context.WithValue(req.Context(), tokenScopeKey, "admin")
	rec := httptest.NewRecorder()
	handler(rec, req.WithContext(ctx))

	if rec.Code != http.StatusOK {
		t.Fatalf("admin should be allowed on readonly endpoint, got %d", rec.Code)
	}
}

func TestRequireScope_ReadonlyOnReadEndpoint(t *testing.T) {
	handler := requireScope("readonly", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	ctx := context.WithValue(req.Context(), tokenScopeKey, "readonly")
	rec := httptest.NewRecorder()
	handler(rec, req.WithContext(ctx))

	if rec.Code != http.StatusOK {
		t.Fatalf("readonly should be allowed on readonly endpoint, got %d", rec.Code)
	}
}

func TestRequireScope_ReadonlyOnAdminEndpoint(t *testing.T) {
	handler := requireScope("admin", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("POST", "/test", nil)
	ctx := context.WithValue(req.Context(), tokenScopeKey, "readonly")
	rec := httptest.NewRecorder()
	handler(rec, req.WithContext(ctx))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("readonly should be denied on admin endpoint, got %d", rec.Code)
	}
}

func TestRequireScope_NoScope_AuthDisabled(t *testing.T) {
	handler := requireScope("readonly", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// No scope in context means auth was disabled — should be allowed.
	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("auth-disabled (no scope) should be allowed, got %d", rec.Code)
	}
}

func TestRequireScope_UnknownScope(t *testing.T) {
	handler := requireScope("admin", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	ctx := context.WithValue(req.Context(), tokenScopeKey, "bogus")
	rec := httptest.NewRecorder()
	handler(rec, req.WithContext(ctx))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("unknown scope should be denied, got %d", rec.Code)
	}
}

// ─────────────────────────────────────────────────────────
// Scope enforcement through full routing
// ─────────────────────────────────────────────────────────

func TestScopeEnforcement_ReadonlyTokenOnWriteEndpoints(t *testing.T) {
	mock := &mockDB{
		tokens: []mockToken{
			{plaintext: "ro-token", token: db.Token{Scope: "readonly"}},
		},
	}
	s := newTestServer(mock, false)
	mux := s.setupHTTPRoutes()

	writeEndpoints := []struct {
		method string
		path   string
		body   string
	}{
		{"PUT", "/api/v1/hosts/h1/tags", `{"env":"prod"}`},
		{"PATCH", "/api/v1/hosts/h1/tags", `{"env":"prod"}`},
		{"DELETE", "/api/v1/hosts/h1/tags/env", ""},
		{"POST", "/api/v1/tokens", `{"name":"t","scope":"admin"}`},
		{"DELETE", "/api/v1/tokens/mytoken", ""},
		{"POST", "/api/v1/exec", `{"agent_id":"a1","command":"id"}`},
		{"POST", "/api/v1/put_file", `{"agent_id":"a1","dest_path":"/tmp/f","content":"x"}`},
		{"POST", "/api/v1/fetch_file", `{"agent_id":"a1","src_path":"/tmp/f"}`},
	}

	for _, ep := range writeEndpoints {
		var body *strings.Reader
		if ep.body != "" {
			body = strings.NewReader(ep.body)
		} else {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(ep.method, ep.path, body)
		req.Header.Set("Authorization", "Bearer ro-token")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: expected 403 for readonly token, got %d", ep.method, ep.path, rec.Code)
		}
	}
}

func TestScopeEnforcement_ReadonlyTokenOnReadEndpoints(t *testing.T) {
	mock := &mockDB{
		tokens: []mockToken{
			{plaintext: "ro-token", token: db.Token{Scope: "readonly"}},
		},
	}
	s := newTestServer(mock, false)
	mux := s.setupHTTPRoutes()

	readEndpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/api/v1/hosts"},
		{"GET", "/api/v1/hosts/h1"},
		{"GET", "/api/v1/hosts/h1/facts"},
		{"GET", "/api/v1/queries"},
		{"GET", "/api/v1/exec_log"},
		{"GET", "/api/v1/inventory"},
	}

	for _, ep := range readEndpoints {
		req := httptest.NewRequest(ep.method, ep.path, nil)
		req.Header.Set("Authorization", "Bearer ro-token")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code == http.StatusForbidden {
			t.Errorf("%s %s: readonly token should not get 403", ep.method, ep.path)
		}
	}
}

// ─────────────────────────────────────────────────────────
// Handler tests
// ─────────────────────────────────────────────────────────

func TestHealthz(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	mux := s.setupHTTPRoutes()

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("expected 'ok', got %q", rec.Body.String())
	}
}

func TestListHosts(t *testing.T) {
	mock := &mockDB{
		agents: []db.Agent{
			{ID: "a1", Hostname: "web1", Online: true},
			{ID: "a2", Hostname: "web2", Online: false},
		},
	}
	s := newTestServer(mock, true)
	mux := s.setupHTTPRoutes()

	req := httptest.NewRequest("GET", "/api/v1/hosts", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var agents []db.Agent
	json.NewDecoder(rec.Body).Decode(&agents)
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(agents))
	}
}

func TestGetHost_NotFound(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	mux := s.setupHTTPRoutes()

	req := httptest.NewRequest("GET", "/api/v1/hosts/nonexistent", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestGetHost_Found(t *testing.T) {
	mock := &mockDB{
		agents: []db.Agent{
			{ID: "a1", Hostname: "web1", Online: true},
		},
	}
	s := newTestServer(mock, true)
	mux := s.setupHTTPRoutes()

	req := httptest.NewRequest("GET", "/api/v1/hosts/a1", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestCreateToken_MissingName(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	mux := s.setupHTTPRoutes()

	body := strings.NewReader(`{"scope":"admin"}`)
	req := httptest.NewRequest("POST", "/api/v1/tokens", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestCreateToken_Success(t *testing.T) {
	s := newTestServer(&mockDB{}, true)
	mux := s.setupHTTPRoutes()

	body := strings.NewReader(`{"name":"ci","scope":"readonly"}`)
	req := httptest.NewRequest("POST", "/api/v1/tokens", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}

	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp["name"] != "ci" || resp["scope"] != "readonly" || resp["token"] == "" {
		t.Fatalf("unexpected response: %v", resp)
	}
}

func TestInventory_OnlineOnly(t *testing.T) {
	mock := &mockDB{
		agents: []db.Agent{
			{ID: "a1", Hostname: "web1", Online: true, OS: "linux", Arch: "amd64", Tags: map[string]string{"env": "prod"}},
			{ID: "a2", Hostname: "web2", Online: false, OS: "linux", Arch: "amd64", Tags: map[string]string{}},
		},
		facts: map[string][]db.Fact{},
	}
	s := newTestServer(mock, true)
	mux := s.setupHTTPRoutes()

	req := httptest.NewRequest("GET", "/api/v1/inventory", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var inv map[string]any
	json.NewDecoder(rec.Body).Decode(&inv)

	// Only web1 should appear (online), not web2 (offline).
	meta := inv["_meta"].(map[string]any)
	hostvars := meta["hostvars"].(map[string]any)
	if _, ok := hostvars["web1"]; !ok {
		t.Error("expected web1 in inventory")
	}
	if _, ok := hostvars["web2"]; ok {
		t.Error("web2 (offline) should not be in inventory")
	}

	// Check tag grouping.
	tagGroup, ok := inv["tag_env_prod"].(map[string]any)
	if !ok {
		t.Fatal("expected tag_env_prod group")
	}
	hosts := tagGroup["hosts"].([]any)
	if len(hosts) != 1 || hosts[0] != "web1" {
		t.Errorf("expected [web1] in tag_env_prod, got %v", hosts)
	}
}

func TestFlattenInto(t *testing.T) {
	src := map[string]any{
		"os_info": map[string]any{
			"os":      "linux",
			"version": "6.1",
		},
		"hostname": "web1",
		"cpu": map[string]any{
			"info": map[string]any{
				"cores": 4,
			},
		},
	}

	dst := map[string]any{}
	flattenInto(dst, "", src)

	tests := map[string]any{
		"os_info.os":      "linux",
		"os_info.version": "6.1",
		"hostname":        "web1",
		"cpu.info.cores":  4,
	}
	for k, want := range tests {
		got, ok := dst[k]
		if !ok {
			t.Errorf("missing key %q", k)
		} else if got != want {
			t.Errorf("flattenInto[%q] = %v, want %v", k, got, want)
		}
	}

	// Nested maps should not appear as values.
	for k, v := range dst {
		if _, isMap := v.(map[string]any); isMap {
			t.Errorf("key %q still has nested map value", k)
		}
	}
}

func TestSanitizeGroupName(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"prod", "prod"},
		{"us-east-1", "us_east_1"},
		{"web.server", "web_server"},
		{"ABC123", "ABC123"},
		{"a b/c", "a_b_c"},
	}
	for _, tt := range tests {
		got := sanitizeGroupName(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeGroupName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
