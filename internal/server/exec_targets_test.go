// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/query"
	"github.com/atgreen/dirq/internal/signutil"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// resolveExecTargets decides which machines a command runs on. Getting
// it wrong is silent — the command simply lands on the wrong fleet — so
// every case below asserts the exact target set, never just its size.

// ─────────────────────────────────────────────────────────
// Fixtures
// ─────────────────────────────────────────────────────────

// execFleet is a small mixed fleet: two prod web hosts, one staging web
// host, and one prod host with exec disabled.
func execFleet() []db.Agent {
	return []db.Agent{
		{ID: "a1", Hostname: "web-01", Tags: map[string]string{"env": "prod"}, Online: true, ExecEnabled: true},
		{ID: "a2", Hostname: "web-02", Tags: map[string]string{"env": "prod"}, Online: true, ExecEnabled: true},
		{ID: "a3", Hostname: "web-03", Tags: map[string]string{"env": "staging"}, Online: true, ExecEnabled: true},
		{ID: "a4", Hostname: "db-01", Tags: map[string]string{"env": "prod"}, Online: true, ExecEnabled: false},
	}
}

func targetIDs(targets []db.Agent) []string {
	ids := make([]string, len(targets))
	for i, a := range targets {
		ids[i] = a.ID
	}
	sort.Strings(ids)
	return ids
}

// resolve parses queryStr and runs it through resolveExecTargets,
// returning the sorted matched IDs.
func resolve(t *testing.T, s *Server, queryStr string, timeout int) ([]string, int) {
	t.Helper()
	parsed, err := query.Parse(queryStr)
	if err != nil {
		t.Fatalf("parse %q: %v", queryStr, err)
	}
	targets, unresolved, err := s.resolveExecTargets(context.Background(), queryStr, parsed, timeout)
	if err != nil {
		t.Fatalf("resolveExecTargets(%q): %v", queryStr, err)
	}
	return targetIDs(targets), unresolved
}

// withSigner gives the server a real Ed25519 signing key, which
// dispatchQuery requires before it will broadcast anything.
func withSigner(t *testing.T, s *Server) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing.key")
	pubFile := filepath.Join(dir, "signing.pub")
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(priv)), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	signer, err := signutil.LoadSigner(signutil.Config{PrivateKeyFile: keyFile, PublicKeyFile: pubFile})
	if err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}
	s.signer = signer
}

// fakeZoneLeader registers a stream that answers every dispatched query
// on behalf of the agents in `succeed`. Agents absent from the map are
// answered with Success=false, mirroring a real "no match" reply;
// agents listed in `silent` are never answered at all, so the
// dispatcher has to time them out.
func fakeZoneLeader(t *testing.T, s *Server, succeed map[string]bool, silent map[string]bool) {
	t.Helper()
	as := &agentStream{agentID: "zl-1", send: make(chan *pb.ServerMessage, 16)}
	s.mu.Lock()
	s.streams["zl-1"] = as
	s.mu.Unlock()

	done := make(chan struct{})
	t.Cleanup(func() { close(done) })

	go func() {
		for {
			select {
			case <-done:
				return
			case msg := <-as.send:
				qr := msg.GetQueryRequest()
				if qr == nil {
					continue
				}
				for _, id := range qr.TargetAgentIds {
					if silent[id] {
						continue
					}
					s.handleQueryResult(&pb.QueryResult{
						QueryId: qr.QueryId,
						AgentId: id,
						Success: succeed[id],
					})
				}
			}
		}
	}()
}

// ─────────────────────────────────────────────────────────
// Record-level filtering: tags, hostnames, and exec capability
// ─────────────────────────────────────────────────────────

func TestResolveExecTargets_RecordConditions(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			// No WHERE clause targets the whole fleet — minus the agent
			// that has exec disabled.
			name:  "no conditions targets every exec-enabled agent",
			query: `SELECT hostname`,
			want:  []string{"a1", "a2", "a3"},
		},
		{
			name:  "tag condition narrows to matching tags",
			query: `SELECT hostname WHERE tag.env = 'prod'`,
			want:  []string{"a1", "a2"},
		},
		{
			// The case behind dirq-8cp: a hostname-only query carries no
			// tag condition, so any caller gating on tags alone would
			// skip filtering and target the whole fleet.
			name:  "hostname condition narrows to one host",
			query: `SELECT hostname WHERE hostname = 'web-01'`,
			want:  []string{"a1"},
		},
		{
			name:  "hostname IN narrows to the listed hosts",
			query: `SELECT hostname WHERE hostname IN ('web-01', 'web-03')`,
			want:  []string{"a1", "a3"},
		},
		{
			name:  "hostname LIKE narrows by prefix",
			query: `SELECT hostname WHERE hostname LIKE 'web-%'`,
			want:  []string{"a1", "a2", "a3"},
		},
		{
			name:  "hostname and tag intersect",
			query: `SELECT hostname WHERE hostname LIKE 'web-%' AND tag.env = 'prod'`,
			want:  []string{"a1", "a2"},
		},
		{
			name:  "exec-disabled agent is excluded even on a direct hostname match",
			query: `SELECT hostname WHERE hostname = 'db-01'`,
			want:  []string{},
		},
		{
			name:  "no match yields no targets rather than the whole fleet",
			query: `SELECT hostname WHERE tag.env = 'qa'`,
			want:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(&mockDB{agents: execFleet()}, true)
			got, unresolved := resolve(t, s, tt.query, 1)
			if !equalIDs(got, tt.want) {
				t.Errorf("targets = %v, want %v", got, tt.want)
			}
			if unresolved != 0 {
				t.Errorf("unresolved = %d, want 0 (no field conditions)", unresolved)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────
// Field conditions: intersection with the resolution query
// ─────────────────────────────────────────────────────────

func TestResolveExecTargets_FieldConditionIntersects(t *testing.T) {
	s := newTestServer(&mockDB{agents: execFleet()}, true)
	withSigner(t, s)
	// a1 reports a match, a2 does not.
	fakeZoneLeader(t, s, map[string]bool{"a1": true}, nil)

	got, unresolved := resolve(t, s, `SELECT hostname WHERE tag.env = 'prod' AND os_info.os = 'linux'`, 5)

	if !equalIDs(got, []string{"a1"}) {
		t.Errorf("targets = %v, want [a1] — the field condition must narrow the tag-matched set", got)
	}
	if unresolved != 0 {
		t.Errorf("unresolved = %d, want 0 — every target answered", unresolved)
	}
}

func TestResolveExecTargets_FieldConditionCannotWidenBeyondRecordFilter(t *testing.T) {
	s := newTestServer(&mockDB{agents: execFleet()}, true)
	withSigner(t, s)
	// Every agent claims a match, but the tag filter already excluded
	// a3 and a4 — the intersection must not let them back in.
	fakeZoneLeader(t, s, map[string]bool{"a1": true, "a2": true, "a3": true, "a4": true}, nil)

	got, _ := resolve(t, s, `SELECT hostname WHERE tag.env = 'prod' AND os_info.os = 'linux'`, 5)

	if !equalIDs(got, []string{"a1", "a2"}) {
		t.Errorf("targets = %v, want [a1 a2] — intersection must not widen the record filter", got)
	}
}

func TestResolveExecTargets_SilentAgentsAreDroppedAndCounted(t *testing.T) {
	s := newTestServer(&mockDB{agents: execFleet()}, true)
	withSigner(t, s)
	// a1 answers; a2 never does. A silent agent must not be assumed to
	// match — it is dropped and surfaced through `unresolved` so the
	// caller can report partial coverage instead of claiming N/N.
	fakeZoneLeader(t, s, map[string]bool{"a1": true}, map[string]bool{"a2": true})

	got, unresolved := resolve(t, s, `SELECT hostname WHERE tag.env = 'prod' AND os_info.os = 'linux'`, 1)

	if !equalIDs(got, []string{"a1"}) {
		t.Errorf("targets = %v, want [a1]", got)
	}
	if unresolved != 1 {
		t.Errorf("unresolved = %d, want 1 (a2 never answered)", unresolved)
	}
}

func TestResolveExecTargets_ResolutionFailureFallsBackToRecordFilter(t *testing.T) {
	// No signer, so dispatch fails and the resolution query returns an
	// error. The fallback must be the tag/hostname-filtered set — NOT
	// the unfiltered fleet, which would be a silent over-broad exec.
	s := newTestServer(&mockDB{agents: execFleet()}, true)

	got, unresolved := resolve(t, s, `SELECT hostname WHERE tag.env = 'prod' AND os_info.os = 'linux'`, 1)

	if !equalIDs(got, []string{"a1", "a2"}) {
		t.Errorf("targets = %v, want [a1 a2] — a failed resolution must not widen the target set", got)
	}
	if unresolved != 0 {
		t.Errorf("unresolved = %d, want 0 — nothing was resolved, so nothing is outstanding", unresolved)
	}
}

func TestResolveExecTargets_FieldOnlyQueryStillResolves(t *testing.T) {
	// A field-only query has no tag or hostname condition, so the
	// record pre-filter is skipped entirely and the resolution query
	// is the only thing narrowing the fleet.
	s := newTestServer(&mockDB{agents: execFleet()}, true)
	withSigner(t, s)
	fakeZoneLeader(t, s, map[string]bool{"a2": true, "a4": true}, nil)

	got, _ := resolve(t, s, `SELECT hostname WHERE os_info.os = 'linux'`, 5)

	// a4 matched the field condition but has exec disabled.
	if !equalIDs(got, []string{"a2"}) {
		t.Errorf("targets = %v, want [a2]", got)
	}
}

// ─────────────────────────────────────────────────────────
// Request decoding
// ─────────────────────────────────────────────────────────

func TestDecodeExecMultiRequest_Rejects(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode int
		wantMsg  string
	}{
		{"malformed JSON", `{"query":`, http.StatusBadRequest, "invalid JSON"},
		{"no command and no script", `{"query":"SELECT hostname"}`, http.StatusBadRequest, "command or script is required"},
		{"no query", `{"command":"uptime"}`, http.StatusBadRequest, "query is required"},
		{"undecodable stdin", `{"query":"SELECT hostname","command":"cat","stdin":"!!!"}`, http.StatusBadRequest, "invalid base64 stdin"},
		{"undecodable script", `{"query":"SELECT hostname","script":"!!!"}`, http.StatusBadRequest, "invalid base64 script"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/v1/exec_multi", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()

			_, _, _, ok := decodeExecMultiRequest(rec, req)

			if ok {
				t.Fatal("decodeExecMultiRequest returned ok=true, want false")
			}
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if !strings.Contains(rec.Body.String(), tt.wantMsg) {
				t.Errorf("body = %q, want it to mention %q", rec.Body.String(), tt.wantMsg)
			}
		})
	}
}

func TestDecodeExecMultiRequest_DecodesPayloads(t *testing.T) {
	body, err := json.Marshal(execMultiRequest{
		Query:   "SELECT hostname WHERE tag.env = 'prod'",
		Command: "cat",
		Stdin:   base64.StdEncoding.EncodeToString([]byte("hello stdin")),
		Script:  base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\necho hi\n")),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/v1/exec_multi", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()

	got, stdin, script, ok := decodeExecMultiRequest(rec, req)

	if !ok {
		t.Fatalf("decodeExecMultiRequest returned ok=false, body=%q", rec.Body.String())
	}
	if got.Query != "SELECT hostname WHERE tag.env = 'prod'" {
		t.Errorf("query = %q", got.Query)
	}
	if string(stdin) != "hello stdin" {
		t.Errorf("stdin = %q, want %q", stdin, "hello stdin")
	}
	if string(script) != "#!/bin/sh\necho hi\n" {
		t.Errorf("script = %q", script)
	}
}

// TestDecodeExecMultiRequest_ScriptOnlyIsAccepted pins that a script
// with no command is a valid request — the two fields are alternatives,
// not a required pair.
func TestDecodeExecMultiRequest_ScriptOnlyIsAccepted(t *testing.T) {
	body := `{"query":"SELECT hostname","script":"` + base64.StdEncoding.EncodeToString([]byte("echo hi")) + `"}`
	req := httptest.NewRequest("POST", "/api/v1/exec_multi", strings.NewReader(body))
	rec := httptest.NewRecorder()

	_, _, script, ok := decodeExecMultiRequest(rec, req)

	if !ok {
		t.Fatalf("script-only request rejected: %q", rec.Body.String())
	}
	if string(script) != "echo hi" {
		t.Errorf("script = %q, want %q", script, "echo hi")
	}
}

// equalIDs compares two sorted ID slices, treating nil and empty as equal.
func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
