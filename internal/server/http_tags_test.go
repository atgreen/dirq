// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atgreen/dirq/internal/db"
)

// Tags decide which hosts a query, exec, or deploy selects, so a tag
// write that silently loses or keeps the wrong key redirects real
// commands at real machines. These handlers are also the public API
// surface, which means their status codes are a contract.

// callHandler runs one handler with the given path values populated, the
// way the router would.
func callHandler(h http.HandlerFunc, method, body string, pathValues map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/v1/test", strings.NewReader(body))
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func tagFleet() []db.Agent {
	return []db.Agent{
		{ID: "a1", Hostname: "web-01", Online: true, Tags: map[string]string{"env": "prod", "role": "web"}},
		{ID: "a2", Hostname: "web-02", Online: true, Tags: nil}, // registered without tags
	}
}

// decodeAgent reads an agent from a handler's JSON response.
func decodeAgent(t *testing.T, rec *httptest.ResponseRecorder) db.Agent {
	t.Helper()
	var a db.Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatalf("response %q: %v", rec.Body.String(), err)
	}
	return a
}

// ─────────────────────────────────────────────────────────
// Set (PUT) — replaces the whole tag map
// ─────────────────────────────────────────────────────────

func TestHandleSetTags_ReplacesEveryTag(t *testing.T) {
	mock := &mockDB{agents: tagFleet()}
	s := newTestServer(mock, true)

	rec := callHandler(s.handleSetTags, "PUT", `{"env":"staging"}`, map[string]string{"id": "a1"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	got := decodeAgent(t, rec).Tags
	// "role" must be gone: PUT replaces, it does not merge.
	if len(got) != 1 || got["env"] != "staging" {
		t.Errorf("tags = %v, want exactly {env:staging}", got)
	}
}

func TestHandleSetTags_ResolvesByHostnameToo(t *testing.T) {
	mock := &mockDB{agents: tagFleet()}
	s := newTestServer(mock, true)

	// Operators type hostnames, not UUIDs.
	rec := callHandler(s.handleSetTags, "PUT", `{"env":"qa"}`, map[string]string{"id": "web-01"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if mock.agents[0].Tags["env"] != "qa" {
		t.Errorf("agent a1 tags = %v, want env=qa", mock.agents[0].Tags)
	}
}

func TestHandleSetTags_ClearsTagsOnEmptyObject(t *testing.T) {
	mock := &mockDB{agents: tagFleet()}
	s := newTestServer(mock, true)

	rec := callHandler(s.handleSetTags, "PUT", `{}`, map[string]string{"id": "a1"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := decodeAgent(t, rec).Tags; len(got) != 0 {
		t.Errorf("tags = %v, want empty — an empty object clears them", got)
	}
}

func TestHandleSetTags_Rejects(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		body     string
		wantCode int
		wantMsg  string
	}{
		{"unknown host", "nope", `{"env":"prod"}`, http.StatusNotFound, "host not found"},
		{"malformed JSON", "a1", `{"env":`, http.StatusBadRequest, "invalid JSON"},
		{"JSON array, not an object", "a1", `["env"]`, http.StatusBadRequest, "invalid JSON"},
		{"nested value, not a string", "a1", `{"env":{"deep":1}}`, http.StatusBadRequest, "invalid JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(&mockDB{agents: tagFleet()}, true)
			rec := callHandler(s.handleSetTags, "PUT", tt.body, map[string]string{"id": tt.id})
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.wantMsg) {
				t.Errorf("body = %q, want it to mention %q", rec.Body.String(), tt.wantMsg)
			}
		})
	}
}

func TestHandleSetTags_SurfacesAStoreFailure(t *testing.T) {
	// A write that never reached the database must not answer 200 —
	// the caller would believe the fleet was retagged.
	s := newTestServer(&mockDB{agents: tagFleet(), errUpdateTags: errors.New("disk on fire")}, true)

	rec := callHandler(s.handleSetTags, "PUT", `{"env":"prod"}`, map[string]string{"id": "a1"})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "disk on fire") {
		t.Errorf("body = %q, want the store's error", rec.Body.String())
	}
}

// ─────────────────────────────────────────────────────────
// Merge (PATCH) — adds and updates without removing
// ─────────────────────────────────────────────────────────

func TestHandleMergeTags_KeepsTagsItWasNotGiven(t *testing.T) {
	mock := &mockDB{agents: tagFleet()}
	s := newTestServer(mock, true)

	rec := callHandler(s.handleMergeTags, "PATCH", `{"env":"staging","tier":"edge"}`, map[string]string{"id": "a1"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	got := decodeAgent(t, rec).Tags
	if got["env"] != "staging" {
		t.Errorf("env = %q, want staging (updated)", got["env"])
	}
	if got["tier"] != "edge" {
		t.Errorf("tier = %q, want edge (added)", got["tier"])
	}
	// The whole difference from PUT: untouched keys survive.
	if got["role"] != "web" {
		t.Errorf("role = %q, want web — merge must not drop existing tags", got["role"])
	}
}

// TestHandleMergeTags_OntoAnAgentWithNoTags covers the agent that
// registered without any tags at all. Its tag map is nil, and writing to
// a nil map panics in Go.
func TestHandleMergeTags_OntoAnAgentWithNoTags(t *testing.T) {
	mock := &mockDB{agents: tagFleet()}
	s := newTestServer(mock, true)

	rec := callHandler(s.handleMergeTags, "PATCH", `{"env":"prod"}`, map[string]string{"id": "a2"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := decodeAgent(t, rec).Tags; got["env"] != "prod" {
		t.Errorf("tags = %v, want env=prod", got)
	}
}

func TestHandleMergeTags_Rejects(t *testing.T) {
	s := newTestServer(&mockDB{agents: tagFleet()}, true)

	if rec := callHandler(s.handleMergeTags, "PATCH", `{"env":"prod"}`, map[string]string{"id": "nope"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown host: status = %d, want 404", rec.Code)
	}
	if rec := callHandler(s.handleMergeTags, "PATCH", `not json`, map[string]string{"id": "a1"}); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status = %d, want 400", rec.Code)
	}
}

func TestHandleMergeTags_SurfacesAStoreFailure(t *testing.T) {
	s := newTestServer(&mockDB{agents: tagFleet(), errUpdateTags: errors.New("store down")}, true)

	rec := callHandler(s.handleMergeTags, "PATCH", `{"env":"prod"}`, map[string]string{"id": "a1"})

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// ─────────────────────────────────────────────────────────
// Delete one tag
// ─────────────────────────────────────────────────────────

func TestHandleDeleteTag_RemovesOnlyTheNamedKey(t *testing.T) {
	mock := &mockDB{agents: tagFleet()}
	s := newTestServer(mock, true)

	rec := callHandler(s.handleDeleteTag, "DELETE", "", map[string]string{"id": "a1", "key": "role"})

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	got := mock.agents[0].Tags
	if _, still := got["role"]; still {
		t.Error("role survived its own deletion")
	}
	if got["env"] != "prod" {
		t.Errorf("env = %q, want prod — deleting one tag must not disturb the others", got["env"])
	}
}

func TestHandleDeleteTag_UnknownKeyIsNotAnError(t *testing.T) {
	mock := &mockDB{agents: tagFleet()}
	s := newTestServer(mock, true)

	// Deleting what is not there is the desired end state, so it succeeds.
	rec := callHandler(s.handleDeleteTag, "DELETE", "", map[string]string{"id": "a1", "key": "absent"})

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(mock.agents[0].Tags) != 2 {
		t.Errorf("tags = %v, want both original tags intact", mock.agents[0].Tags)
	}
}

func TestHandleDeleteTag_OnAnAgentWithNoTags(t *testing.T) {
	s := newTestServer(&mockDB{agents: tagFleet()}, true)

	rec := callHandler(s.handleDeleteTag, "DELETE", "", map[string]string{"id": "a2", "key": "env"})

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestHandleDeleteTag_Rejects(t *testing.T) {
	if rec := callHandler(newTestServer(&mockDB{agents: tagFleet()}, true).handleDeleteTag,
		"DELETE", "", map[string]string{"id": "nope", "key": "env"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown host: status = %d, want 404", rec.Code)
	}

	s := newTestServer(&mockDB{agents: tagFleet(), errUpdateTags: errors.New("store down")}, true)
	if rec := callHandler(s.handleDeleteTag, "DELETE", "", map[string]string{"id": "a1", "key": "env"}); rec.Code != http.StatusInternalServerError {
		t.Errorf("store failure: status = %d, want 500", rec.Code)
	}
}

// ─────────────────────────────────────────────────────────
// Tokens
// ─────────────────────────────────────────────────────────

func TestHandleListTokens(t *testing.T) {
	mock := &mockDB{tokens: []mockToken{
		{plaintext: "secret-1", token: db.Token{Name: "ci", Scope: "admin"}},
		{plaintext: "secret-2", token: db.Token{Name: "readonly", Scope: "read"}},
	}}
	s := newTestServer(mock, true)

	rec := callHandler(s.handleListTokens, "GET", "", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var tokens []db.Token
	if err := json.Unmarshal(rec.Body.Bytes(), &tokens); err != nil {
		t.Fatalf("response %q: %v", rec.Body.String(), err)
	}
	if len(tokens) != 2 {
		t.Fatalf("got %d tokens, want 2", len(tokens))
	}
	// The listing must never carry the secret itself.
	if body := rec.Body.String(); strings.Contains(body, "secret-1") || strings.Contains(body, "secret-2") {
		t.Errorf("token listing leaked a plaintext token: %s", body)
	}
}

func TestHandleListTokens_SurfacesAStoreFailure(t *testing.T) {
	s := newTestServer(&mockDB{errListTokens: errors.New("store down")}, true)

	if rec := callHandler(s.handleListTokens, "GET", "", nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestHandleDeleteToken(t *testing.T) {
	mock := &mockDB{}
	s := newTestServer(mock, true)

	rec := callHandler(s.handleDeleteToken, "DELETE", "", map[string]string{"name": "ci"})

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if mock.lastDelete != "ci" {
		t.Errorf("deleted %q, want %q", mock.lastDelete, "ci")
	}
}

func TestHandleDeleteToken_SurfacesAStoreFailure(t *testing.T) {
	s := newTestServer(&mockDB{errDeleteToken: errors.New("store down")}, true)

	if rec := callHandler(s.handleDeleteToken, "DELETE", "", map[string]string{"name": "ci"}); rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// ─────────────────────────────────────────────────────────
// Row projection
// ─────────────────────────────────────────────────────────

func TestProjectRow(t *testing.T) {
	src := map[string]any{
		"hostname":       "web-01",
		"os_info.os":     "linux",
		"disk.pct_used":  91.5,
		"cpu.core_count": 8,
	}

	t.Run("keeps only the selected fields, in any order", func(t *testing.T) {
		got := projectRow(src, []string{"hostname", "disk.pct_used"})
		if len(got) != 2 || got["hostname"] != "web-01" || got["disk.pct_used"] != 91.5 {
			t.Errorf("projectRow = %v, want just hostname and disk.pct_used", got)
		}
	})

	t.Run("a field the agent did not report is omitted, not nulled", func(t *testing.T) {
		got := projectRow(src, []string{"hostname", "memory.total"})
		if _, present := got["memory.total"]; present {
			t.Errorf("projectRow = %v, want memory.total absent rather than a nil entry", got)
		}
		if len(got) != 1 {
			t.Errorf("projectRow = %v, want only hostname", got)
		}
	})

	t.Run("no fields selected yields an empty row", func(t *testing.T) {
		if got := projectRow(src, nil); len(got) != 0 {
			t.Errorf("projectRow = %v, want empty", got)
		}
	})
}
