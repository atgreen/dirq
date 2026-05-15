// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/atgreen/dirq/internal/config"
	"github.com/atgreen/dirq/internal/db"
	"github.com/atgreen/dirq/internal/signutil"
	"github.com/atgreen/dirq/internal/tlsutil"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// Config holds server configuration.
type Config struct {
	GRPCAddr            string // e.g. ":50051"
	HTTPAddr            string // e.g. ":8080"
	DBURL               string // PostgreSQL connection string
	PodID               string // unique identifier for this server pod
	MaxZoneLeaders      int    // topology: max zone leaders (default 50)
	MaxChildrenPerNode  int    // topology: max children per node (default 50)
	AuthDisabled        bool   // DIRQ_AUTH_DISABLED=true to allow anonymous API access
	RegistrationSecret  string // pre-shared secret for agent registration
	FileCfg             *config.File // parsed config file (for TLS/signing fallback)
}

// Server is the DirQ server.
type Server struct {
	pb.UnimplementedDirQServerServer

	cfg     Config
	topoCfg TopologyConfig
	db      db.DB
	log     *slog.Logger
	grpcSv  *grpc.Server
	httpSv  *http.Server
	signer  *signutil.Signer

	// Connected zone leaders: agentID -> stream
	mu      sync.RWMutex
	streams map[string]*agentStream

	// Phase 2: exec sessions pending response
	execMu       sync.RWMutex
	execSessions map[string]*execSession

	// Session tokens: agentID -> token (generated during registration,
	// validated when an agent opens a stream).
	sessionMu     sync.RWMutex
	sessionTokens map[string]string

	// Agents currently being reassigned by the rebalancer. Their
	// disconnect from the old parent is expected — don't mark offline.
	reassigningMu sync.Mutex
	reassigning   map[string]time.Time

	// Dampening: tracks agents that were demoted but bounced back to
	// a direct server connection.  Prevents the rebalancer from
	// re-demoting them every cycle.
	demoteMu       sync.Mutex
	demoteCooldown map[string]demoteRecord

	// Bounded worker pool for fact upserts (#7). Prevents unbounded
	// goroutine creation when thousands of query results arrive at once.
	factCh chan factUpsert
}

type factUpsert struct {
	agentID string
	data    map[string]any
}

type agentStream struct {
	agentID      string
	capabilities []string
	send         chan *pb.ServerMessage
	cancel       context.CancelFunc
	reassigned   bool // true if this agent was demoted/reassigned — don't mark offline on stream close
}

// New creates a new DirQ server.
func New(cfg Config, database db.DB, log *slog.Logger) *Server {
	topoCfg := DefaultTopologyConfig()
	if cfg.MaxZoneLeaders > 0 {
		topoCfg.MaxZoneLeaders = cfg.MaxZoneLeaders
	}
	if cfg.MaxChildrenPerNode > 0 {
		topoCfg.MaxChildrenPerNode = cfg.MaxChildrenPerNode
	}

	return &Server{
		cfg:            cfg,
		topoCfg:        topoCfg,
		db:             database,
		log:            log,
		streams:        make(map[string]*agentStream),
		execSessions:   make(map[string]*execSession),
		sessionTokens:  make(map[string]string),
		reassigning:    make(map[string]time.Time),
		demoteCooldown: make(map[string]demoteRecord),
		factCh:         make(chan factUpsert, 4096),
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
			MinTime:             30 * time.Second,
			PermitWithoutStream: true,
		}),
	}

	// TLS is on by default. Auto-generates self-signed certs if none provided.
	tlsCfg := tlsutil.ConfigFromEnv(s.cfg.FileCfg)
	tlsCfg, err := tlsutil.EnsureCerts(tlsCfg, "server", s.log)
	if err != nil {
		return fmt.Errorf("TLS setup: %w", err)
	}
	if tlsCfg.Enabled() {
		creds, err := tlsutil.ServerCredentials(tlsCfg)
		if err != nil {
			return fmt.Errorf("TLS credentials: %w", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
	} else {
		// Only reached when tls_disabled=true
	}

	signer, err := signutil.EnsureServerSigner(signutil.ConfigFromEnv(s.cfg.FileCfg), s.log)
	if err != nil {
		return fmt.Errorf("signing setup: %w", err)
	}
	s.signer = signer

	s.grpcSv = grpc.NewServer(grpcOpts...)
	pb.RegisterDirQServerServer(s.grpcSv, s)

	grpcLis, err := net.Listen("tcp", s.cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}

	// HTTP/HTTPS server (REST API + Web UI)
	mux := s.setupHTTPRoutes()
	s.httpSv = &http.Server{
		Addr:    s.cfg.HTTPAddr,
		Handler: mux,
	}

	// Use HTTPS when TLS certs are available.
	if tlsCfg.Enabled() && tlsCfg.CertFile != "" {
		httpTLS, err := tls.LoadX509KeyPair(tlsCfg.CertFile, tlsCfg.KeyFile)
		if err != nil {
			return fmt.Errorf("HTTP TLS setup: %w", err)
		}
		s.httpSv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{httpTLS},
			MinVersion:   tls.VersionTLS12,
		}
		s.log.Info("HTTPS enabled for REST API", "addr", s.cfg.HTTPAddr)
	} else {
		s.log.Warn("REST API running on plain HTTP")
	}

	httpLis, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("http listen: %w", err)
	}

	// Register this pod in the peers table
	if err := s.db.RegisterServerPeer(ctx, s.cfg.PodID, s.cfg.GRPCAddr); err != nil {
		s.log.Warn("failed to register server peer", "error", err)
	}

	// Start the stale-agent reaper, topology rebalancer, and fact workers.
	go s.startReaper(ctx)
	go s.startRebalancer(ctx)
	s.startFactWorkers(ctx, 8)

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
	go func() {
		if s.httpSv.TLSConfig != nil {
			errCh <- s.httpSv.ServeTLS(httpLis, "", "") // certs already in TLSConfig
		} else {
			errCh <- s.httpSv.Serve(httpLis)
		}
	}()

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
