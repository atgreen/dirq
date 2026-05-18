// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"net"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// ─────────────────────────────────────────────────────────
// requiresClientCert
// ─────────────────────────────────────────────────────────

func TestRequiresClientCert(t *testing.T) {
	tests := []struct {
		method string
		want   bool
	}{
		{"/dirq.v1.DirQServer/Register", false},
		{"/dirq.v1.DirQServer/AgentStream", true},
		{"/dirq.v1.DirQServer/RequestPeers", true},
	}
	for _, tt := range tests {
		if got := requiresClientCert(tt.method); got != tt.want {
			t.Errorf("requiresClientCert(%q) = %v, want %v", tt.method, got, tt.want)
		}
	}
}

// ─────────────────────────────────────────────────────────
// peerCerts
// ─────────────────────────────────────────────────────────

func TestPeerCerts_NoPeer(t *testing.T) {
	ctx := context.Background()
	if certs := peerCerts(ctx); certs != nil {
		t.Fatalf("expected nil, got %v", certs)
	}
}

func TestPeerCerts_NoTLS(t *testing.T) {
	// Peer present but without TLS auth info.
	p := &peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}}
	ctx := peer.NewContext(context.Background(), p)
	if certs := peerCerts(ctx); certs != nil {
		t.Fatalf("expected nil, got %v", certs)
	}
}

func TestPeerCerts_EmptyVerifiedChains(t *testing.T) {
	// Peer with TLS info but no verified chains — the panic bug we fixed.
	p := &peer.Peer{
		Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				VerifiedChains: nil,
			},
		},
	}
	ctx := peer.NewContext(context.Background(), p)
	if certs := peerCerts(ctx); certs != nil {
		t.Fatalf("expected nil, got %v", certs)
	}
}

// ─────────────────────────────────────────────────────────
// TLSCNFromContext
// ─────────────────────────────────────────────────────────

func TestTLSCNFromContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKeyTLSCN, "agent-42")
	cn, ok := TLSCNFromContext(ctx)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if cn != "agent-42" {
		t.Fatalf("expected 'agent-42', got %q", cn)
	}

	// Missing value.
	cn2, ok2 := TLSCNFromContext(context.Background())
	if ok2 {
		t.Fatal("expected ok=false for empty context")
	}
	if cn2 != "" {
		t.Fatalf("expected empty string, got %q", cn2)
	}
}

// ─────────────────────────────────────────────────────────
// enforceMTLS
// ─────────────────────────────────────────────────────────

func newMTLSTestServer(mtls bool) *Server {
	return &Server{
		mtlsEnabled: mtls,
		log:         slog.Default(),
	}
}

// peerCtxWithCert builds a context containing a peer with a TLS client cert
// bearing the given CN.
func peerCtxWithCert(cn string) context.Context {
	cert := &x509.Certificate{
		Subject: pkix.Name{CommonName: cn},
	}
	p := &peer.Peer{
		Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				VerifiedChains: [][]*x509.Certificate{{cert}},
			},
		},
	}
	return peer.NewContext(context.Background(), p)
}

func TestEnforceMTLS_RegisterAllowed(t *testing.T) {
	s := newMTLSTestServer(true)
	ctx := context.Background() // no client cert
	_, err := s.enforceMTLS(ctx, "/dirq.v1.DirQServer/Register")
	if err != nil {
		t.Fatalf("Register should pass without client cert, got: %v", err)
	}
}

func TestEnforceMTLS_AgentStreamRejected(t *testing.T) {
	s := newMTLSTestServer(true)
	ctx := context.Background() // no client cert
	_, err := s.enforceMTLS(ctx, "/dirq.v1.DirQServer/AgentStream")
	if err == nil {
		t.Fatal("AgentStream without client cert should be rejected")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", st.Code())
	}
}

func TestEnforceMTLS_Disabled(t *testing.T) {
	s := newMTLSTestServer(false)
	ctx := context.Background() // no client cert

	methods := []string{
		"/dirq.v1.DirQServer/Register",
		"/dirq.v1.DirQServer/AgentStream",
		"/dirq.v1.DirQServer/RequestPeers",
	}
	for _, m := range methods {
		if _, err := s.enforceMTLS(ctx, m); err != nil {
			t.Errorf("mtlsEnabled=false: %s should pass, got: %v", m, err)
		}
	}
}

func TestEnforceMTLS_WithCert_SetsCN(t *testing.T) {
	s := newMTLSTestServer(true)
	ctx := peerCtxWithCert("my-agent")

	out, err := s.enforceMTLS(ctx, "/dirq.v1.DirQServer/AgentStream")
	if err != nil {
		t.Fatalf("should pass with valid cert, got: %v", err)
	}
	cn, ok := TLSCNFromContext(out)
	if !ok || cn != "my-agent" {
		t.Fatalf("expected CN 'my-agent', got %q (ok=%v)", cn, ok)
	}
}
