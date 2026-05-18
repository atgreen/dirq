// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"crypto/x509"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const (
	// ctxKeyTLSCN stores the CN from the client's TLS certificate.
	ctxKeyTLSCN contextKey = "tlsCN"
)

// TLSCNFromContext extracts the TLS client certificate CN from the context.
func TLSCNFromContext(ctx context.Context) (string, bool) {
	cn, ok := ctx.Value(ctxKeyTLSCN).(string)
	return cn, ok
}

// peerCerts extracts verified client certificates from the gRPC peer info.
func peerCerts(ctx context.Context) []*x509.Certificate {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil
	}
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return nil
	}
	return tlsInfo.State.VerifiedChains[0]
}

// requiresClientCert returns true for methods that require mTLS.
// Register is exempt — it's the bootstrap entry point before the agent
// has a client cert.
func requiresClientCert(fullMethod string) bool {
	return fullMethod != "/dirq.v1.DirQServer/Register"
}

// mtlsUnaryInterceptor enforces client certificate presence on all unary
// RPCs except Register.
func (s *Server) mtlsUnaryInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	ctx, err := s.enforceMTLS(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// mtlsStreamInterceptor enforces client certificate presence on all
// streaming RPCs (AgentStream).
func (s *Server) mtlsStreamInterceptor(
	srv any,
	ss grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) error {
	ctx, err := s.enforceMTLS(ss.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	return handler(srv, &wrappedStream{ServerStream: ss, ctx: ctx})
}

// enforceMTLS checks for a valid client certificate when required.
// If a cert is present, the CN is stored in the context.
func (s *Server) enforceMTLS(ctx context.Context, fullMethod string) (context.Context, error) {
	certs := peerCerts(ctx)
	if len(certs) > 0 {
		cn := certs[0].Subject.CommonName
		ctx = context.WithValue(ctx, ctxKeyTLSCN, cn)
	} else if s.mtlsEnabled && requiresClientCert(fullMethod) {
		s.log.Warn("mTLS: rejected connection without client cert", "method", fullMethod)
		return ctx, status.Error(codes.Unauthenticated, "client certificate required")
	}
	return ctx, nil
}

// wrappedStream wraps a grpc.ServerStream to override its Context().
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context {
	return w.ctx
}
