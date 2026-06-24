// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atgreen/dirq/internal/db"
)

func TestCheckAAPBinding(t *testing.T) {
	bound := db.Token{Name: "svc-prod", AAPUsers: []string{"svc-ansible-prod", "svc-ansible-dba"}}
	unbound := db.Token{Name: "legacy"}

	cases := []struct {
		name        string
		tok         db.Token
		aapUser     string
		requireBind bool
		wantErr     bool
		errContains string
	}{
		{name: "bound token, matching user", tok: bound, aapUser: "svc-ansible-prod", wantErr: false},
		{name: "bound token, second matching user", tok: bound, aapUser: "svc-ansible-dba", wantErr: false},
		{name: "bound token, mismatched user", tok: bound, aapUser: "svc-ansible-nonprod", wantErr: true, errContains: "may not act as aap_user"},
		{name: "bound token, empty user", tok: bound, aapUser: "", wantErr: true, errContains: "aap_user is required"},
		{name: "unbound token, gate off", tok: unbound, aapUser: "anyone", requireBind: false, wantErr: false},
		{name: "unbound token, gate on", tok: unbound, aapUser: "anyone", requireBind: true, wantErr: true, errContains: "not bound to an aap_user"},
		{name: "unbound token, gate off, no user", tok: unbound, aapUser: "", requireBind: false, wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAAPBinding(tc.tok, tc.aapUser, tc.requireBind)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr && tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
				t.Fatalf("error %q does not contain %q", err, tc.errContains)
			}
		})
	}
}

// TestExecBindingRejectedBeforeDispatch proves a mismatched aap_user is denied
// at the API with 403 before the handler ever looks up the agent or signs a
// message. The agent does not exist in the mock, so a 404 would mean the
// binding check was skipped; a 403 proves it fired first.
func TestExecBindingRejectedBeforeDispatch(t *testing.T) {
	s := newTestServer(&mockDB{}, false)
	s.cfg.RequireAAPBinding = true

	body := `{"agent_id":"agent-x","command":"id","aap_user":"svc-ansible-prod"}`
	req := httptest.NewRequest("POST", "/api/v1/exec", strings.NewReader(body))
	// Token bound to a different account than the request asserts.
	tok := db.Token{Name: "svc-nonprod", Scope: "admin", AAPUsers: []string{"svc-ansible-nonprod"}}
	req = req.WithContext(context.WithValue(req.Context(), tokenCtxKey, tok))

	rec := httptest.NewRecorder()
	s.handleExecCommand(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (binding denial before agent lookup), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "may not act as aap_user") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

// TestExecBindingAllowsMatching confirms a matching aap_user passes the binding
// and proceeds (here it then 404s because the mock has no such agent — proving
// it got past the gate to the agent-lookup stage).
func TestExecBindingAllowsMatching(t *testing.T) {
	s := newTestServer(&mockDB{}, false)
	s.cfg.RequireAAPBinding = true

	body := `{"agent_id":"agent-x","command":"id","aap_user":"svc-ansible-prod"}`
	req := httptest.NewRequest("POST", "/api/v1/exec", strings.NewReader(body))
	tok := db.Token{Name: "svc-prod", Scope: "admin", AAPUsers: []string{"svc-ansible-prod"}}
	req = req.WithContext(context.WithValue(req.Context(), tokenCtxKey, tok))

	rec := httptest.NewRecorder()
	s.handleExecCommand(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Fatalf("matching aap_user should pass the binding, got 403: %s", rec.Body.String())
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (agent absent) after passing the gate, got %d", rec.Code)
	}
}

// TestCreateTokenPassesAAPUsers confirms the create-token handler forwards the
// allowlist to the DB layer.
func TestCreateTokenPassesAAPUsers(t *testing.T) {
	mock := &mockDB{}
	s := newTestServer(mock, true)

	body := `{"name":"svc-prod","scope":"admin","aap_users":["svc-ansible-prod"]}`
	req := httptest.NewRequest("POST", "/api/v1/tokens", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleCreateToken(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(mock.lastCreateAAPUsers) != 1 || mock.lastCreateAAPUsers[0] != "svc-ansible-prod" {
		t.Fatalf("aap_users not forwarded to DB: %v", mock.lastCreateAAPUsers)
	}
}
