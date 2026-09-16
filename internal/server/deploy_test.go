// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atgreen/dirq/internal/db"
)

// Characterization tests for the broadcast deploy endpoint.  They pin the
// externally visible contract — status codes, validation precedence, and the
// NDJSON header line — so the handler can be restructured without silently
// moving behavior.  The server has no signer configured, so any request that
// reaches the broadcast stage stops at a "sign failed" result line; that is
// deliberate, and keeps every case fast and deterministic.

func deployAgent(id, hostname string, execEnabled bool, tags map[string]string) db.Agent {
	return db.Agent{ID: id, Hostname: hostname, Online: true, ExecEnabled: execEnabled, Tags: tags}
}

// postDeploy runs one request through handleBroadcastDeploy and returns the
// status and the raw NDJSON body.
func postDeploy(t *testing.T, s *Server, body string, tok *db.Token) (int, []string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/deploy", strings.NewReader(body))
	if tok != nil {
		req = req.WithContext(context.WithValue(req.Context(), tokenCtxKey, *tok))
	}
	rec := httptest.NewRecorder()
	s.handleBroadcastDeploy(rec, req)

	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return rec.Code, lines
}

func TestBroadcastDeployValidation(t *testing.T) {
	content := base64.StdEncoding.EncodeToString([]byte("package-bytes"))

	cases := []struct {
		name     string
		body     string
		wantCode int
		wantBody string
	}{
		{
			name:     "invalid JSON",
			body:     `{not json`,
			wantCode: http.StatusBadRequest,
			wantBody: "invalid JSON",
		},
		{
			name:     "missing content",
			body:     `{"query":"SELECT hostname","dest_path":"/tmp/p.rpm","install_command":"rpm -i"}`,
			wantCode: http.StatusBadRequest,
			wantBody: "content is required",
		},
		{
			name:     "missing install_command",
			body:     `{"query":"SELECT hostname","dest_path":"/tmp/p.rpm","content":"` + content + `"}`,
			wantCode: http.StatusBadRequest,
			wantBody: "install_command is required",
		},
		{
			name:     "missing dest_path",
			body:     `{"query":"SELECT hostname","install_command":"rpm -i","content":"` + content + `"}`,
			wantCode: http.StatusBadRequest,
			wantBody: "dest_path is required",
		},
		{
			name:     "missing query",
			body:     `{"dest_path":"/tmp/p.rpm","install_command":"rpm -i","content":"` + content + `"}`,
			wantCode: http.StatusBadRequest,
			wantBody: "query is required",
		},
		{
			name:     "invalid base64 content",
			body:     `{"query":"SELECT hostname","dest_path":"/tmp/p.rpm","install_command":"rpm -i","content":"!!!not-base64!!!"}`,
			wantCode: http.StatusBadRequest,
			wantBody: "invalid base64 content",
		},
		{
			name:     "unparseable query",
			body:     `{"query":"DELETE FROM everything","dest_path":"/tmp/p.rpm","install_command":"rpm -i","content":"` + content + `"}`,
			wantCode: http.StatusBadRequest,
			wantBody: "query parse error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(&mockDB{}, true)
			code, lines := postDeploy(t, s, tc.body, nil)
			if code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %v)", code, tc.wantCode, lines)
			}
			if !strings.Contains(strings.Join(lines, "\n"), tc.wantBody) {
				t.Fatalf("body %v does not contain %q", lines, tc.wantBody)
			}
		})
	}
}

// TestBroadcastDeployBindingPrecedence pins the validation order: the AAP
// binding check runs after content/install_command but BEFORE dest_path and
// query.  A request that is both unauthorized and missing dest_path must
// answer 403, not 400 — clients distinguish "you may not do this" from "you
// asked wrong", and reordering the checks would flip that.
func TestBroadcastDeployBindingPrecedence(t *testing.T) {
	content := base64.StdEncoding.EncodeToString([]byte("package-bytes"))
	bound := db.Token{Name: "svc-prod", AAPUsers: []string{"svc-ansible-prod"}}

	t.Run("unauthorized and missing dest_path answers 403", func(t *testing.T) {
		s := newTestServer(&mockDB{}, false)
		body := `{"query":"SELECT hostname","install_command":"rpm -i","content":"` + content + `","aap_user":"svc-ansible-nonprod"}`
		code, lines := postDeploy(t, s, body, &bound)
		if code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %v)", code, lines)
		}
	})

	t.Run("unauthorized and missing content answers 400", func(t *testing.T) {
		s := newTestServer(&mockDB{}, false)
		body := `{"query":"SELECT hostname","dest_path":"/tmp/p.rpm","install_command":"rpm -i","aap_user":"svc-ansible-nonprod"}`
		code, lines := postDeploy(t, s, body, &bound)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %v)", code, lines)
		}
	})
}

func TestBroadcastDeployTargetSelection(t *testing.T) {
	content := base64.StdEncoding.EncodeToString([]byte("package-bytes"))
	deployBody := func(q string) string {
		return `{"query":"` + q + `","dest_path":"/tmp/p.rpm","install_command":"rpm -i","content":"` + content + `"}`
	}

	agents := []db.Agent{
		deployAgent("a1", "web01", true, map[string]string{"env": "prod"}),
		deployAgent("a2", "web02", true, map[string]string{"env": "dev"}),
		deployAgent("a3", "db01", false, map[string]string{"env": "prod"}), // exec disabled
	}

	cases := []struct {
		name        string
		agents      []db.Agent
		query       string
		wantTargets float64
	}{
		{name: "no agents", agents: nil, query: "SELECT hostname", wantTargets: 0},
		{name: "exec-disabled agents are excluded", agents: []db.Agent{agents[2]}, query: "SELECT hostname", wantTargets: 0},
		{name: "no where clause targets all exec-enabled", agents: agents, query: "SELECT hostname", wantTargets: 2},
		{name: "tag condition narrows to matching agents", agents: agents, query: "SELECT hostname WHERE tag.env = 'prod'", wantTargets: 1},
		{name: "non-matching tag condition targets none", agents: agents, query: "SELECT hostname WHERE tag.env = 'staging'", wantTargets: 0},
		// Characterizes current (incorrect) behavior — see dirq-8cp.  A
		// hostname condition contains no tag condition, so the filter is
		// skipped entirely and every exec-enabled agent is targeted.
		{name: "hostname condition is ignored and targets all (dirq-8cp)", agents: agents, query: "SELECT hostname WHERE hostname = 'web01'", wantTargets: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(&mockDB{agents: tc.agents}, true)
			code, lines := postDeploy(t, s, deployBody(tc.query), nil)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %v)", code, lines)
			}
			if len(lines) == 0 {
				t.Fatal("expected a header line, got empty body")
			}

			var header map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
				t.Fatalf("header line %q: %v", lines[0], err)
			}
			if header["type"] != "header" {
				t.Fatalf("first line is not a header: %v", header)
			}
			if got := header["total_targets"]; got != tc.wantTargets {
				t.Fatalf("total_targets = %v, want %v", got, tc.wantTargets)
			}

			// With targets and no signer, the handler stops at the signing
			// stage rather than broadcasting.
			if tc.wantTargets > 0 {
				if len(lines) != 2 || !strings.Contains(lines[1], "sign failed") {
					t.Fatalf("expected a sign-failure result line, got %v", lines)
				}
			} else if len(lines) != 1 {
				t.Fatalf("expected header only for zero targets, got %v", lines)
			}
		})
	}
}
