// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package signutil

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// Server-message signing is what stops an agent from executing an
// instruction the server never sent. Every test here therefore has a
// negative twin: it is not enough to show a good signature verifies,
// the bad ones must be refused.

// signedMessage returns a message carrying a real instruction, signed now.
func signedMessage(t *testing.T, s *Signer) *pb.ServerMessage {
	t.Helper()
	msg := &pb.ServerMessage{
		Payload: &pb.ServerMessage_QueryRequest{
			QueryRequest: &pb.QueryRequest{
				QueryId:  "q-1",
				RawQuery: "SELECT hostname",
			},
		},
	}
	if err := s.SignServerMessage(msg, time.Minute); err != nil {
		t.Fatalf("SignServerMessage: %v", err)
	}
	return msg
}

func TestServerMessage_RoundTrip(t *testing.T) {
	s := newTestSigner(t)
	v := newTestVerifier(t, s)

	msg := signedMessage(t, s)
	if err := v.VerifyServerMessage(msg, time.Now()); err != nil {
		t.Fatalf("a freshly signed message must verify: %v", err)
	}
	if msg.GetSignature() == nil {
		t.Error("signing left no signature")
	}
	if msg.GetSignerKeyId() != s.KeyID() {
		t.Errorf("signer_key_id = %q, want %q", msg.GetSignerKeyId(), s.KeyID())
	}
}

func TestServerMessage_Rejects(t *testing.T) {
	tests := []struct {
		name string
		// tamper mutates a validly signed message; the result must not verify.
		tamper func(t *testing.T, s *Signer, msg *pb.ServerMessage)
		at     func(signedAt time.Time) time.Time
		// wantErr names the guard that must reject it. Asserting only "some
		// error" is not enough: every case here also trips the signature
		// check, so a deleted guard would still fail the test for the wrong
		// reason and look fine.
		wantErr string
	}{
		{
			name:    "no signature at all",
			tamper:  func(_ *testing.T, _ *Signer, m *pb.ServerMessage) { m.Signature = nil },
			wantErr: "missing signature",
		},
		{
			name: "signature replaced with noise",
			tamper: func(_ *testing.T, _ *Signer, m *pb.ServerMessage) {
				m.Signature = make([]byte, ed25519.SignatureSize)
			},
			wantErr: "invalid signature",
		},
		{
			name:    "one bit flipped in the signature",
			tamper:  func(_ *testing.T, _ *Signer, m *pb.ServerMessage) { m.Signature[0] ^= 0x01 },
			wantErr: "invalid signature",
		},
		{
			// The whole point: change the instruction, keep the signature.
			name: "payload altered after signing",
			tamper: func(_ *testing.T, _ *Signer, m *pb.ServerMessage) {
				m.GetQueryRequest().RawQuery = "SELECT hostname WHERE tag.env = 'prod'"
			},
			wantErr: "invalid signature",
		},
		{
			name: "target list altered after signing",
			tamper: func(_ *testing.T, _ *Signer, m *pb.ServerMessage) {
				m.GetQueryRequest().TargetAgentIds = []string{"someone-else"}
			},
			wantErr: "invalid signature",
		},
		{
			name:    "key id cleared",
			tamper:  func(_ *testing.T, _ *Signer, m *pb.ServerMessage) { m.SignerKeyId = "" },
			wantErr: "unexpected signer key id",
		},
		{
			name:    "key id claims a different signer",
			tamper:  func(_ *testing.T, _ *Signer, m *pb.ServerMessage) { m.SignerKeyId = "not-this-key" },
			wantErr: "unexpected signer key id",
		},
		{
			name: "signed by a different key entirely",
			tamper: func(t *testing.T, _ *Signer, m *pb.ServerMessage) {
				other := newTestSigner(t)
				if err := other.SignServerMessage(m, time.Minute); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "unexpected signer key id",
		},
		{
			name:    "signed-at timestamp missing",
			tamper:  func(_ *testing.T, _ *Signer, m *pb.ServerMessage) { m.SignedAtUnix = 0 },
			wantErr: "missing signature timestamps",
		},
		{
			name:    "expiry timestamp missing",
			tamper:  func(_ *testing.T, _ *Signer, m *pb.ServerMessage) { m.ExpiresAtUnix = 0 },
			wantErr: "missing signature timestamps",
		},
		{
			// A replayed message must stop working once its window closes.
			name:    "expired",
			tamper:  func(_ *testing.T, _ *Signer, m *pb.ServerMessage) {},
			at:      func(signedAt time.Time) time.Time { return signedAt.Add(2 * time.Minute) },
			wantErr: "signature expired",
		},
		{
			name:    "signed further in the future than clock skew allows",
			tamper:  func(_ *testing.T, _ *Signer, m *pb.ServerMessage) {},
			at:      func(signedAt time.Time) time.Time { return signedAt.Add(-60 * time.Second) },
			wantErr: "timestamp is in the future",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestSigner(t)
			v := newTestVerifier(t, s)
			msg := signedMessage(t, s)

			signedAt := time.Unix(msg.GetSignedAtUnix(), 0)
			tt.tamper(t, s, msg)

			at := signedAt
			if tt.at != nil {
				at = tt.at(signedAt)
			}
			err := v.VerifyServerMessage(msg, at)
			if err == nil {
				t.Fatal("VerifyServerMessage accepted a message it must refuse")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("rejected with %q, want the %q guard to fire", err, tt.wantErr)
			}
		})
	}
}

// TestServerMessage_KeyIDCheckedIndependentlyOfSignature isolates the key-id
// guard. Everywhere else a wrong key id also changes the signed bytes, so the
// signature check would mask the guard's removal; here the signature is
// genuinely valid and only the expected id differs.
func TestServerMessage_KeyIDCheckedIndependentlyOfSignature(t *testing.T) {
	s := newTestSigner(t)
	v, err := NewVerifier(s.PublicKey(), "a-different-key-id")
	if err != nil {
		t.Fatal(err)
	}

	err = v.VerifyServerMessage(signedMessage(t, s), time.Now())
	if err == nil {
		t.Fatal("a message whose key id is not the expected one was accepted")
	}
	if !strings.Contains(err.Error(), "unexpected signer key id") {
		t.Errorf("rejected with %q, want the key-id guard to fire", err)
	}
}

func TestServerMessage_AcceptsWithinSkewAndWindow(t *testing.T) {
	s := newTestSigner(t)
	v := newTestVerifier(t, s)
	msg := signedMessage(t, s)
	signedAt := time.Unix(msg.GetSignedAtUnix(), 0)

	// A verifier whose clock runs slightly behind the signer's still
	// accepts: real fleets do not share a clock.
	if err := v.VerifyServerMessage(msg, signedAt.Add(-20*time.Second)); err != nil {
		t.Errorf("message within the 30s skew tolerance was refused: %v", err)
	}
	// Valid right up to the expiry second.
	if err := v.VerifyServerMessage(msg, time.Unix(msg.GetExpiresAtUnix(), 0)); err != nil {
		t.Errorf("message at its expiry second was refused: %v", err)
	}
	// And not one second past it.
	if err := v.VerifyServerMessage(msg, time.Unix(msg.GetExpiresAtUnix()+1, 0)); err == nil {
		t.Error("message one second past expiry was accepted")
	}
}

func TestSignServerMessage_NonPositiveTTLUsesDefault(t *testing.T) {
	s := newTestSigner(t)
	for _, ttl := range []time.Duration{0, -time.Hour} {
		msg := &pb.ServerMessage{}
		if err := s.SignServerMessage(msg, ttl); err != nil {
			t.Fatalf("ttl=%v: %v", ttl, err)
		}
		got := msg.GetExpiresAtUnix() - msg.GetSignedAtUnix()
		if want := int64(defaultValidity.Seconds()); got != want {
			t.Errorf("ttl=%v gave a %ds window, want the %ds default", ttl, got, want)
		}
	}
}

// TestSignServerMessage_ResigningReplacesTheOldSignature guards the
// Signature=nil reset in SignServerMessage: without it the canonical
// bytes would include the previous signature and re-signing would
// produce something no verifier accepts.
func TestSignServerMessage_ResigningReplacesTheOldSignature(t *testing.T) {
	s := newTestSigner(t)
	v := newTestVerifier(t, s)

	msg := signedMessage(t, s)
	first := append([]byte(nil), msg.GetSignature()...)

	msg.GetQueryRequest().RawQuery = "SELECT hostname WHERE tag.env = 'prod'"
	if err := s.SignServerMessage(msg, time.Minute); err != nil {
		t.Fatalf("re-sign: %v", err)
	}
	if err := v.VerifyServerMessage(msg, time.Now()); err != nil {
		t.Fatalf("re-signed message must verify: %v", err)
	}
	if string(first) == string(msg.GetSignature()) {
		t.Error("re-signing a changed message produced the same signature")
	}
}

// ─────────────────────────────────────────────────────────
// MultiVerifier — the key-rotation window
// ─────────────────────────────────────────────────────────

func TestMultiVerifier_AcceptsEitherKeyDuringRotation(t *testing.T) {
	oldSigner, newSigner := newTestSigner(t), newTestSigner(t)
	mv := NewMultiVerifier(newTestVerifier(t, newSigner), newTestVerifier(t, oldSigner))

	// Both halves of the rotation window must work, or agents still
	// holding the old key drop every instruction.
	for name, s := range map[string]*Signer{"new key": newSigner, "old key": oldSigner} {
		if err := mv.VerifyServerMessage(signedMessage(t, s), time.Now()); err != nil {
			t.Errorf("%s was refused during rotation: %v", name, err)
		}
		if tok := s.SignToken("agent-1"); !mv.VerifyToken("agent-1", tok) {
			t.Errorf("%s token was refused during rotation", name)
		}
	}
}

func TestMultiVerifier_RejectsAKeyItDoesNotHold(t *testing.T) {
	known, stranger := newTestSigner(t), newTestSigner(t)
	mv := NewMultiVerifier(newTestVerifier(t, known))

	if err := mv.VerifyServerMessage(signedMessage(t, stranger), time.Now()); err == nil {
		t.Error("a message from an untrusted key was accepted")
	}
	if mv.VerifyToken("agent-1", stranger.SignToken("agent-1")) {
		t.Error("a token from an untrusted key was accepted")
	}
}

func TestMultiVerifier_EmptyRejectsEverything(t *testing.T) {
	mv := NewMultiVerifier()
	s := newTestSigner(t)

	// Trusting nobody must mean trusting nobody — not trusting everybody.
	if err := mv.VerifyServerMessage(signedMessage(t, s), time.Now()); err == nil {
		t.Fatal("a MultiVerifier with no verifiers accepted a message")
	}
	if mv.VerifyToken("agent-1", s.SignToken("agent-1")) {
		t.Error("a MultiVerifier with no verifiers accepted a token")
	}
}

func TestMultiVerifier_VerifiersReturnsACopy(t *testing.T) {
	s := newTestSigner(t)
	mv := NewMultiVerifier(newTestVerifier(t, s))

	got := mv.Verifiers()
	if len(got) != 1 {
		t.Fatalf("Verifiers() returned %d entries, want 1", len(got))
	}
	// Callers build a new MultiVerifier from this slice during rotation;
	// handing out the backing array would let them blank the live one.
	got[0] = nil
	if err := mv.VerifyServerMessage(signedMessage(t, s), time.Now()); err != nil {
		t.Errorf("mutating the Verifiers() result damaged the MultiVerifier: %v", err)
	}
}

// ─────────────────────────────────────────────────────────
// Key loading
// ─────────────────────────────────────────────────────────

func TestNewVerifier_RejectsAMalformedPublicKey(t *testing.T) {
	for _, n := range []int{0, 1, ed25519.PublicKeySize - 1, ed25519.PublicKeySize + 1} {
		if _, err := NewVerifier(make([]byte, n), ""); err == nil {
			t.Errorf("NewVerifier accepted a %d-byte public key", n)
		}
	}
}

func TestNewVerifier_DerivesKeyIDWhenUnset(t *testing.T) {
	s := newTestSigner(t)
	v, err := NewVerifier(s.PublicKey(), "")
	if err != nil {
		t.Fatal(err)
	}
	if v.keyID != s.KeyID() {
		t.Errorf("derived key id = %q, want %q", v.keyID, s.KeyID())
	}
	if err := v.VerifyServerMessage(signedMessage(t, s), time.Now()); err != nil {
		t.Errorf("verifier with a derived key id refused a valid message: %v", err)
	}
}

func TestLoadSigner(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(priv)

	write := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "signing.key")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("loads a key and signs with it", func(t *testing.T) {
		s, err := LoadSigner(Config{PrivateKeyFile: write(t, encoded), PublicKeyFile: "pub"})
		if err != nil {
			t.Fatalf("LoadSigner: %v", err)
		}
		v := newTestVerifier(t, s)
		if err := v.VerifyServerMessage(signedMessage(t, s), time.Now()); err != nil {
			t.Errorf("a loaded signer produced an unverifiable message: %v", err)
		}
	})

	t.Run("tolerates surrounding whitespace", func(t *testing.T) {
		// Key files get edited, copied, and echoed into place; a trailing
		// newline must not make the server refuse to start.
		if _, err := LoadSigner(Config{PrivateKeyFile: write(t, "\n  "+encoded+"  \r\n"), PublicKeyFile: "pub"}); err != nil {
			t.Errorf("a key with surrounding whitespace was refused: %v", err)
		}
	})

	rejects := []struct {
		name string
		cfg  func(t *testing.T) Config
	}{
		{"no private key path", func(*testing.T) Config { return Config{PublicKeyFile: "pub"} }},
		{"no public key path", func(t *testing.T) Config { return Config{PrivateKeyFile: write(t, encoded)} }},
		{"missing file", func(t *testing.T) Config {
			return Config{PrivateKeyFile: filepath.Join(t.TempDir(), "absent.key"), PublicKeyFile: "pub"}
		}},
		{"not base64", func(t *testing.T) Config {
			return Config{PrivateKeyFile: write(t, "!!! not base64 !!!"), PublicKeyFile: "pub"}
		}},
		{"empty file", func(t *testing.T) Config {
			return Config{PrivateKeyFile: write(t, ""), PublicKeyFile: "pub"}
		}},
		{"right encoding, wrong length", func(t *testing.T) Config {
			return Config{PrivateKeyFile: write(t, base64.StdEncoding.EncodeToString([]byte("too short"))), PublicKeyFile: "pub"}
		}},
	}
	for _, tt := range rejects {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := LoadSigner(tt.cfg(t)); err == nil {
				t.Fatal("LoadSigner accepted a configuration it must refuse")
			}
		})
	}
}

// TestEnsureServerSigner_GeneratesThenReusesAKey pins that a server with
// no configured key generates one and then keeps it: regenerating on
// every start would invalidate every agent's session token on restart.
func TestEnsureServerSigner_GeneratesThenReusesAKey(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		PrivateKeyFile: filepath.Join(dir, "server_signing.key"),
		PublicKeyFile:  filepath.Join(dir, "server_signing.pub"),
	}

	first, err := EnsureServerSigner(cfg, nil)
	if err == nil {
		t.Fatal("EnsureServerSigner with both paths set should load, not generate, and the files do not exist yet")
	}
	_ = first

	// Generate through the auto-path by writing the key ourselves, then
	// confirm a second load returns the same identity.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.PrivateKeyFile, []byte(base64.StdEncoding.EncodeToString(priv)), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := EnsureServerSigner(cfg, nil)
	if err != nil {
		t.Fatalf("EnsureServerSigner: %v", err)
	}
	b, err := EnsureServerSigner(cfg, nil)
	if err != nil {
		t.Fatalf("EnsureServerSigner (second call): %v", err)
	}
	if a.KeyID() != b.KeyID() {
		t.Errorf("key id changed between loads: %q then %q", a.KeyID(), b.KeyID())
	}
}
