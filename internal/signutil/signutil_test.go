// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package signutil

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

func newTestSigner(t *testing.T) *Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := newSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newTestVerifier(t *testing.T, s *Signer) *Verifier {
	t.Helper()
	v, err := NewVerifier(s.PublicKey(), s.KeyID())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSignToken_Valid(t *testing.T) {
	s := newTestSigner(t)
	token := s.SignToken("agent-123")

	if !s.VerifyToken("agent-123", token) {
		t.Fatal("signer should verify its own token")
	}

	v := newTestVerifier(t, s)
	if !v.VerifyToken("agent-123", token) {
		t.Fatal("verifier should verify token from same key")
	}
}

func TestSignToken_WrongAgentID(t *testing.T) {
	s := newTestSigner(t)
	token := s.SignToken("agent-123")

	if s.VerifyToken("agent-456", token) {
		t.Fatal("token should not verify for a different agent ID")
	}
}

func TestSignToken_DifferentKey(t *testing.T) {
	s1 := newTestSigner(t)
	s2 := newTestSigner(t)
	token := s1.SignToken("agent-123")

	if s2.VerifyToken("agent-123", token) {
		t.Fatal("token signed by one key should not verify with another")
	}
}

func TestSignToken_UniquePerCall(t *testing.T) {
	s := newTestSigner(t)
	t1 := s.signTokenAt("agent-123", time.Now())
	t2 := s.signTokenAt("agent-123", time.Now().Add(1*time.Second))

	if t1 == t2 {
		t.Fatal("tokens issued at different times should differ")
	}
	if !s.VerifyToken("agent-123", t1) || !s.VerifyToken("agent-123", t2) {
		t.Fatal("both tokens should be valid")
	}
}

func TestSignToken_Expired(t *testing.T) {
	s := newTestSigner(t)
	token := s.signTokenAt("agent-123", time.Now().Add(-25*time.Hour))

	if s.VerifyToken("agent-123", token) {
		t.Fatal("token older than 24h should be rejected")
	}
}

func TestSignToken_JustBeforeExpiry(t *testing.T) {
	s := newTestSigner(t)
	token := s.signTokenAt("agent-123", time.Now().Add(-23*time.Hour))

	if !s.VerifyToken("agent-123", token) {
		t.Fatal("token within 24h should be accepted")
	}
}

func TestSignToken_FutureDated(t *testing.T) {
	s := newTestSigner(t)
	token := s.signTokenAt("agent-123", time.Now().Add(5*time.Minute))

	if s.VerifyToken("agent-123", token) {
		t.Fatal("token with timestamp >30s in the future should be rejected")
	}
}

func TestSignToken_FutureWithinSkew(t *testing.T) {
	s := newTestSigner(t)
	token := s.signTokenAt("agent-123", time.Now().Add(10*time.Second))

	if !s.VerifyToken("agent-123", token) {
		t.Fatal("token with timestamp <=30s in the future should be accepted")
	}
}

func TestSignToken_MalformedNoColon(t *testing.T) {
	s := newTestSigner(t)
	if s.VerifyToken("agent-123", "notavalidtoken") {
		t.Fatal("token without colon should be rejected")
	}
}

func TestSignToken_MalformedBadTimestamp(t *testing.T) {
	s := newTestSigner(t)
	if s.VerifyToken("agent-123", "abc:notanumber") {
		t.Fatal("token with non-numeric timestamp should be rejected")
	}
}

func TestSignToken_MalformedBadBase64(t *testing.T) {
	s := newTestSigner(t)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	if s.VerifyToken("agent-123", "!!!notbase64!!!:"+ts) {
		t.Fatal("token with invalid base64 should be rejected")
	}
}

func TestSignToken_TamperedSignature(t *testing.T) {
	s := newTestSigner(t)
	token := s.SignToken("agent-123")

	// Flip a byte in the signature portion.
	lastColon := len(token) - 1
	for lastColon > 0 && token[lastColon] != ':' {
		lastColon--
	}
	sigBytes, _ := base64.StdEncoding.DecodeString(token[:lastColon])
	sigBytes[0] ^= 0xff
	tampered := base64.StdEncoding.EncodeToString(sigBytes) + token[lastColon:]

	if s.VerifyToken("agent-123", tampered) {
		t.Fatal("tampered signature should be rejected")
	}
}

func TestSignToken_EmptyAgentID(t *testing.T) {
	s := newTestSigner(t)
	token := s.SignToken("")

	if !s.VerifyToken("", token) {
		t.Fatal("empty agent ID token should verify for empty agent ID")
	}
	if s.VerifyToken("agent-123", token) {
		t.Fatal("empty agent ID token should not verify for non-empty agent ID")
	}
}

func TestSignToken_EmptyToken(t *testing.T) {
	s := newTestSigner(t)
	if s.VerifyToken("agent-123", "") {
		t.Fatal("empty token should be rejected")
	}
}
