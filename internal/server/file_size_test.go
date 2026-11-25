// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/wire"
)

// A file past wire.MaxFileBytes cannot cross the mesh in one message, so the
// REST API must refuse it up front rather than hand it to a hop that will
// drop its whole stream over it (dirq-gdj).
func TestFileTransfersRejectOversizeContent(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a body past wire.MaxFileBytes")
	}
	over := base64.StdEncoding.EncodeToString(make([]byte, wire.MaxFileBytes+1))
	s := newTestServer(&mockDB{agents: []db.Agent{deployAgent("a1", "h1", true, nil)}}, true)

	cases := map[string]struct {
		handler http.HandlerFunc
		body    string
	}{
		"put_file": {s.handlePutFile, fmt.Sprintf(`{"agent_id":"a1","dest_path":"/tmp/x","content":%q}`, over)},
		"deploy": {s.handleBroadcastDeploy, fmt.Sprintf(
			`{"query":"SELECT hostname","dest_path":"/tmp/x","install_command":"true","content":%q}`, over)},
	}
	for name, c := range cases {
		rec := httptest.NewRecorder()
		c.handler(rec, httptest.NewRequest("POST", "/", strings.NewReader(c.body)))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: status %d, want 413: %s", name, rec.Code, rec.Body.String())
		}
	}
}
