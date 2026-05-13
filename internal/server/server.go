// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/tlsutil"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// Config holds server configuration.
type Config struct {
	GRPCAddr           string // e.g. ":50051"
	HTTPAddr           string // e.g. ":8080"
	DBURL              string // PostgreSQL connection string
	PodID              string // unique identifier for this server pod
	MaxZoneLeaders     int    // topology: max zone leaders (default 50)
	MaxChildrenPerNode int    // topology: max children per node (default 50)
}

// Server is the DirQ server.
type Server struct {
	pb.UnimplementedDirQServerServer

	cfg     Config
	topoCfg TopologyConfig
	db      *db.DB
	log     *slog.Logger
	grpcSv  *grpc.Server
	httpSv  *http.Server

	// Connected zone leaders: agentID -> stream
	mu      sync.RWMutex
	streams map[string]*agentStream

	// Phase 2: exec sessions pending response
	execMu       sync.RWMutex
	execSessions map[string]*execSession
}

type agentStream struct {
	agentID      string
	capabilities []string
	send         chan *pb.ServerMessage
	cancel       context.CancelFunc
}

// New creates a new DirQ server.
func New(cfg Config, database *db.DB, log *slog.Logger) *Server {
	topoCfg := DefaultTopologyConfig()
	if cfg.MaxZoneLeaders > 0 {
		topoCfg.MaxZoneLeaders = cfg.MaxZoneLeaders
	}
	if cfg.MaxChildrenPerNode > 0 {
		topoCfg.MaxChildrenPerNode = cfg.MaxChildrenPerNode
	}

	return &Server{
		cfg:          cfg,
		topoCfg:      topoCfg,
		db:           database,
		log:          log,
		streams:      make(map[string]*agentStream),
		execSessions: make(map[string]*execSession),
	}
}

// Start starts the gRPC and HTTP servers.
func (s *Server) Start(ctx context.Context) error {
	// Build gRPC server options.
	grpcOpts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	}

	// Add TLS if configured.
	tlsCfg := tlsutil.ConfigFromEnv()
	if tlsCfg.Enabled() {
		creds, err := tlsutil.ServerCredentials(tlsCfg)
		if err != nil {
			return fmt.Errorf("TLS setup: %w", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
		s.log.Info("TLS enabled for gRPC", "cert", tlsCfg.CertFile, "ca", tlsCfg.CAFile)
	} else {
		s.log.Warn("TLS disabled — gRPC connections are unencrypted")
	}

	s.grpcSv = grpc.NewServer(grpcOpts...)
	pb.RegisterDirQServerServer(s.grpcSv, s)

	grpcLis, err := net.Listen("tcp", s.cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}

	// HTTP server (REST API + Web UI)
	mux := s.setupHTTPRoutes()
	s.httpSv = &http.Server{
		Addr:    s.cfg.HTTPAddr,
		Handler: mux,
	}

	httpLis, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("http listen: %w", err)
	}

	// Register this pod in the peers table
	if err := s.db.RegisterServerPeer(ctx, s.cfg.PodID, s.cfg.GRPCAddr); err != nil {
		s.log.Warn("failed to register server peer", "error", err)
	}

	// Start the stale-agent reaper.
	go s.startReaper(ctx)

	capacity := s.topoCfg.MaxZoneLeaders * s.topoCfg.MaxChildrenPerNode * s.topoCfg.MaxChildrenPerNode
	s.log.Info("DirQ server starting",
		"grpc", s.cfg.GRPCAddr,
		"http", s.cfg.HTTPAddr,
		"pod_id", s.cfg.PodID,
		"max_zone_leaders", s.topoCfg.MaxZoneLeaders,
		"max_children", s.topoCfg.MaxChildrenPerNode,
		"max_capacity", capacity,
	)

	errCh := make(chan error, 2)
	go func() { errCh <- s.grpcSv.Serve(grpcLis) }()
	go func() { errCh <- s.httpSv.Serve(httpLis) }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		s.Stop()
		return ctx.Err()
	}
}

// Stop gracefully stops the server.
func (s *Server) Stop() {
	s.log.Info("shutting down")
	s.grpcSv.GracefulStop()
	s.httpSv.Close()
}
