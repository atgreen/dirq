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
		// dirq-8cp: a hostname condition carries no tag condition, so the
		// old deploy-only resolver skipped filtering entirely and installed
		// on every exec-enabled agent. Deploy now shares the exec path's
		// resolver, which honours hostname conditions too.
		{name: "hostname condition narrows to the matching host (dirq-8cp)", agents: agents, query: "SELECT hostname WHERE hostname = 'web01'", wantTargets: 1},
		{name: "hostname condition matching no host targets none", agents: agents, query: "SELECT hostname WHERE hostname = 'nope'", wantTargets: 0},
		{name: "hostname IN narrows to the listed hosts", agents: agents, query: "SELECT hostname WHERE hostname IN ('web01', 'web02')", wantTargets: 2},
		{name: "hostname LIKE narrows by prefix", agents: agents, query: "SELECT hostname WHERE hostname LIKE 'web%'", wantTargets: 2},
		{name: "hostname and tag intersect", agents: agents, query: "SELECT hostname WHERE hostname LIKE 'web%' AND tag.env = 'prod'", wantTargets: 1},
		// An exec-disabled host is excluded even when named directly.
		{name: "hostname naming an exec-disabled host targets none", agents: agents, query: "SELECT hostname WHERE hostname = 'db01'", wantTargets: 0},
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

// TestBroadcastDeployFieldConditions covers the other half of dirq-8cp: a
// deploy whose query filters on an agent-reported field must be intersected
// with the resolution query rather than skipping the filter. Deploy reaches
// this through the same resolver as exec, so these assert the wiring — that
// deploy actually consults it — rather than re-testing the resolver itself.
func TestBroadcastDeployFieldConditions(t *testing.T) {
	content := base64.StdEncoding.EncodeToString([]byte("package-bytes"))
	// A short timeout keeps the silent-agent case from waiting out the
	// default 300s resolution window.
	body := func(q string) string {
		return `{"query":"` + q + `","dest_path":"/tmp/p.rpm","install_command":"rpm -i","content":"` + content + `","timeout":1}`
	}
	fleet := []db.Agent{
		deployAgent("a1", "web01", true, map[string]string{"env": "prod"}),
		deployAgent("a2", "web02", true, map[string]string{"env": "prod"}),
		deployAgent("a3", "web03", true, map[string]string{"env": "dev"}),
	}

	header := func(t *testing.T, lines []string) map[string]any {
		t.Helper()
		if len(lines) == 0 {
			t.Fatal("expected a header line, got empty body")
		}
		var h map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &h); err != nil {
			t.Fatalf("header line %q: %v", lines[0], err)
		}
		return h
	}

	t.Run("field condition narrows the tag-matched set", func(t *testing.T) {
		s := newTestServer(&mockDB{agents: fleet}, true)
		withSigner(t, s)
		// Of the two prod hosts, only a1 reports a matching field.
		fakeZoneLeader(t, s, map[string]bool{"a1": true}, nil)

		code, lines := postDeploy(t, s, body(`SELECT hostname WHERE tag.env = 'prod' AND os_info.os = 'linux'`), nil)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %v)", code, lines)
		}
		h := header(t, lines)
		if got := h["total_targets"]; got != float64(1) {
			t.Errorf("total_targets = %v, want 1 — the field condition must narrow the deploy", got)
		}
		if _, present := h["unresolved_targets"]; present {
			t.Errorf("unresolved_targets present (%v) when every target answered", h["unresolved_targets"])
		}
	})

	t.Run("a field condition cannot widen past the tag filter", func(t *testing.T) {
		s := newTestServer(&mockDB{agents: fleet}, true)
		withSigner(t, s)
		// Every host claims a match, but a3 is not in the tag-matched set.
		fakeZoneLeader(t, s, map[string]bool{"a1": true, "a2": true, "a3": true}, nil)

		_, lines := postDeploy(t, s, body(`SELECT hostname WHERE tag.env = 'prod' AND os_info.os = 'linux'`), nil)
		if got := header(t, lines)["total_targets"]; got != float64(2) {
			t.Errorf("total_targets = %v, want 2 — resolution must not re-admit a tag-excluded host", got)
		}
	})

	t.Run("a silent agent is dropped and surfaced as unresolved", func(t *testing.T) {
		s := newTestServer(&mockDB{agents: fleet}, true)
		withSigner(t, s)
		// a1 answers, a2 never does. Installing on a host that never
		// confirmed it matches is the over-broad deploy dirq-8cp is about.
		fakeZoneLeader(t, s, map[string]bool{"a1": true}, map[string]bool{"a2": true})

		_, lines := postDeploy(t, s, body(`SELECT hostname WHERE tag.env = 'prod' AND os_info.os = 'linux'`), nil)
		h := header(t, lines)
		if got := h["total_targets"]; got != float64(1) {
			t.Errorf("total_targets = %v, want 1", got)
		}
		if got := h["unresolved_targets"]; got != float64(1) {
			t.Errorf("unresolved_targets = %v, want 1 — partial coverage must be reported", got)
		}
	})

	t.Run("a failed resolution falls back to the record filter", func(t *testing.T) {
		// No signer, so the resolution query cannot be dispatched. The
		// fallback must be the tag-matched set, never the whole fleet.
		s := newTestServer(&mockDB{agents: fleet}, true)

		_, lines := postDeploy(t, s, body(`SELECT hostname WHERE tag.env = 'prod' AND os_info.os = 'linux'`), nil)
		if got := header(t, lines)["total_targets"]; got != float64(2) {
			t.Errorf("total_targets = %v, want 2 — a failed resolution must not widen the deploy", got)
		}
	})
}
