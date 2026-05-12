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
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// Config holds server configuration.
type Config struct {
	GRPCAddr string // e.g. ":50051"
	HTTPAddr string // e.g. ":8080"
	DBURL    string // PostgreSQL connection string
	PodID    string // unique identifier for this server pod
}

// Server is the DirQ server.
type Server struct {
	pb.UnimplementedDirQServerServer

	cfg    Config
	db     *db.DB
	log    *slog.Logger
	grpcSv *grpc.Server
	httpSv *http.Server

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
	return &Server{
		cfg:          cfg,
		db:           database,
		log:          log,
		streams:      make(map[string]*agentStream),
		execSessions: make(map[string]*execSession),
	}
}

// Start starts the gRPC and HTTP servers.
func (s *Server) Start(ctx context.Context) error {
	// gRPC server with keepalive to detect dead connections.
	s.grpcSv = grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second, // ping clients every 30s if idle
			Timeout: 10 * time.Second, // wait 10s for pong before closing
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second, // minimum time between client pings
			PermitWithoutStream: true,             // allow pings even with no active streams
		}),
	)
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

	s.log.Info("DirQ server starting",
		"grpc", s.cfg.GRPCAddr,
		"http", s.cfg.HTTPAddr,
		"pod_id", s.cfg.PodID,
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
