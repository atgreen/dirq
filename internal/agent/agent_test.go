// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/atgreen/dirq/internal/config"
	"github.com/atgreen/dirq/internal/signutil"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

func TestIsTargeted(t *testing.T) {
	a := &Agent{agentID: "agent-1"}

	if a.isTargeted(nil) {
		t.Error("empty target list must not match")
	}
	if a.isTargeted([]string{"agent-2", "agent-3"}) {
		t.Error("list without our ID must not match")
	}
	if !a.isTargeted([]string{"agent-2", "agent-1"}) {
		t.Error("list containing our ID must match")
	}
}

func TestPolicyDeniedError(t *testing.T) {
	if got := policyDeniedError(""); got != "policy denied" {
		t.Errorf("empty reason = %q, want \"policy denied\"", got)
	}
	if got := policyDeniedError("no exec at night"); got != "policy denied: no exec at night" {
		t.Errorf("got %q", got)
	}
}

func TestVerifyServerMessageNoVerifier(t *testing.T) {
	a := &Agent{}
	err := a.verifyServerMessage(&pb.ServerMessage{})
	if err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("err = %v, want verifier-not-initialized error", err)
	}
}

func newSigningTestAgent(pinnedKey string) *Agent {
	a := &Agent{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if pinnedKey != "" {
		a.cfg.FileCfg = &config.File{
			Values: map[string]string{"signing_public_key": pinnedKey},
		}
	}
	return a
}

func genKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestSetServerVerifierNoPin(t *testing.T) {
	a := newSigningTestAgent("")
	if err := a.setServerVerifier(genKey(t), "", nil); err != nil {
		t.Fatalf("setServerVerifier: %v", err)
	}
	if a.serverVerifier == nil {
		t.Fatal("serverVerifier not set")
	}
	if _, ok := a.serverVerifier.(*signutil.Verifier); !ok {
		t.Errorf("verifier type = %T, want *signutil.Verifier when no old keys", a.serverVerifier)
	}
}

func TestSetServerVerifierPinnedMatch(t *testing.T) {
	pub := genKey(t)
	a := newSigningTestAgent(base64.StdEncoding.EncodeToString(pub))
	if err := a.setServerVerifier(pub, "", nil); err != nil {
		t.Fatalf("matching pinned key must be accepted: %v", err)
	}
}

func TestSetServerVerifierPinnedMismatch(t *testing.T) {
	a := newSigningTestAgent(base64.StdEncoding.EncodeToString(genKey(t)))
	err := a.setServerVerifier(genKey(t), "", nil)
	if err == nil || !strings.Contains(err.Error(), "does not match pinned key") {
		t.Fatalf("err = %v, want pinned-key mismatch", err)
	}
	if a.serverVerifier != nil {
		t.Error("mismatched key must not install a verifier")
	}
}

func TestSetServerVerifierPinnedInvalidBase64(t *testing.T) {
	a := newSigningTestAgent("not-base64!!!")
	err := a.setServerVerifier(genKey(t), "", nil)
	if err == nil || !strings.Contains(err.Error(), "invalid signing_public_key") {
		t.Fatalf("err = %v, want invalid-pin error", err)
	}
}

func TestSetServerVerifierInvalidKeyLength(t *testing.T) {
	a := newSigningTestAgent("")
	if err := a.setServerVerifier([]byte("short"), "", nil); err == nil {
		t.Fatal("a non-ed25519-sized key must be rejected")
	}
}

func TestSetServerVerifierWithOldKeys(t *testing.T) {
	a := newSigningTestAgent("")
	err := a.setServerVerifier(genKey(t), "", [][]byte{genKey(t)})
	if err != nil {
		t.Fatalf("setServerVerifier: %v", err)
	}
	multi, ok := a.serverVerifier.(*signutil.MultiVerifier)
	if !ok {
		t.Fatalf("verifier type = %T, want *signutil.MultiVerifier during rotation", a.serverVerifier)
	}
	if n := len(multi.Verifiers()); n != 2 {
		t.Errorf("multi-verifier holds %d keys, want 2", n)
	}
}

func TestSetServerVerifierSkipsInvalidOldKeys(t *testing.T) {
	a := newSigningTestAgent("")
	err := a.setServerVerifier(genKey(t), "", [][]byte{[]byte("bogus")})
	if err != nil {
		t.Fatalf("an invalid old key is skipped, not fatal: %v", err)
	}
	multi, ok := a.serverVerifier.(*signutil.MultiVerifier)
	if !ok {
		t.Fatalf("verifier type = %T, want *signutil.MultiVerifier", a.serverVerifier)
	}
	if n := len(multi.Verifiers()); n != 1 {
		t.Errorf("multi-verifier holds %d keys, want 1 (bad old key skipped)", n)
	}
}
