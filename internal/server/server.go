// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	LeaderElection      bool   // when true, only the elected leader marks itself ready
	FileCfg             *config.File // parsed config file (for TLS/signing fallback)

	// Fact batcher knobs. Zero means use defaults.
	FactFlushInterval time.Duration // default 250ms
	FactFlushSize     int           // default 5000  — early flush trigger
	FactStageCap      int           // default 20000 — hard drop ceiling
}

// Server is the DirQ server.
type Server struct {
	pb.UnimplementedDirQServerServer

	cfg       Config
	topoCfg   TopologyConfig
	topology  *MeshTopology         // in-memory authoritative mesh state (see meshtopology.go)
	regBatch  *registrationBatcher  // burst-aware ZL-diverse role assignment
	db        db.DB
	log       *slog.Logger
	grpcSv    *grpc.Server
	httpSv    *http.Server
	signer    *signutil.Signer

	// oldSignerPubKeys holds raw Ed25519 public keys from previous signing keys.
	// Populated when DIRQ_SIGNING_PUB_OLD / signing_pub_old is configured.
	// Included in RegisterResponse and RenewCertResponse so agents can verify
	// messages during a key rotation.
	oldSignerPubKeys [][]byte

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

	// mTLS: when true, all gRPC methods except Register require a
	// valid client certificate. Enabled when the CA key is available.
	mtlsEnabled bool

	// CA for issuing per-agent client certificates during registration.
	tlsCfg       tlsutil.Config
	caCert       *x509.Certificate
	caKey        *ecdsa.PrivateKey
	certReloader *tlsutil.CertReloader

	// Fact-write staging. Query results land here keyed by
	// (agent_id, module) so a burst of upserts for the same key in one
	// flush window collapses to a single row. A batcher goroutine
	// flushes the snapshot via db.BulkUpsertFacts on size or time
	// thresholds — see runFactBatcher.
	factStageMu      sync.Mutex
	factStage        map[factKey]db.FactRow
	factFlushSignal  chan struct{}
	factDropLogged   time.Time // rate-limits the "stage full, dropping" warning

	// Leader election. When LeaderElection is disabled, this stays nil
	// and /readyz unconditionally returns 200 (single-instance mode).
	leader db.Leader
}

// factKey is the dedup key for staged fact upserts. Matches the facts
// table primary key so coalescing in memory is equivalent to letting the
// DB upsert resolve the collision — just much cheaper.
type factKey struct {
	agentID string
	module  string
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

	srv := &Server{
		cfg:             cfg,
		topoCfg:         topoCfg,
		topology:        NewMeshTopology(topoCfg),
		db:              database,
		log:             log,
		streams:         make(map[string]*agentStream),
		execSessions:    make(map[string]*execSession),
		sessionTokens:   make(map[string]string),
		factStage:       make(map[factKey]db.FactRow),
		factFlushSignal: make(chan struct{}, 1),
	}
	srv.regBatch = newRegistrationBatcher(srv)
	return srv
}

// runTopologySnapshotter periodically writes (role, parent_id) pairs
// from the in-memory MeshTopology back to the DB.  The DB is no longer
// the source of truth — this is purely a recovery aid so visibility
// survives a server restart and external tooling can read the agents
// table without an admin RPC.  Writes are best-effort: errors are
// logged but don't affect routing.
func (s *Server) runTopologySnapshotter(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.snapshotTopologyOnce(ctx)
		}
	}
}

func (s *Server) snapshotTopologyOnce(ctx context.Context) {
	// Walk every node and write its current (role, parent_id) to the DB.
	// Fire-and-forget; the topology mutex is released for each write.
	for _, id := range s.topology.allNodeIDs() {
		n, ok := s.topology.Get(id)
		if !ok {
			continue
		}
		if n.Role != "" {
			s.db.SetAgentRole(ctx, n.ID, n.Role)
		}
		s.db.SetAgentParent(ctx, n.ID, n.ParentID)
	}
}

// rehydrateTopology seeds the in-memory MeshTopology from the DB at
// startup so dirq hosts graph isn't blank for the first few seconds
// while agents reconnect.  Routing is rebuilt from live streams anyway,
// so this is purely a visibility aid.
func (s *Server) rehydrateTopology(ctx context.Context) {
	agents, err := s.db.ListAgents(ctx, db.ListAgentsFilter{})
	if err != nil {
		s.log.Warn("rehydrate topology from DB failed", "error", err)
		return
	}
	s.topology.Rehydrate(agents)
	s.log.Info("rehydrated topology from DB", "agents", len(agents))
}

// writeAgentConfig generates /var/lib/dirq/agent.conf with inline TLS certs
// so administrators can copy a single file to onboard new agents.
func (s *Server) writeAgentConfig(tlsCfg tlsutil.Config) {
	outPath := filepath.Join(config.DataDir(), "agent.conf")

	hostname, _ := os.Hostname()
	// Extract just the port from GRPCAddr (which may be "0.0.0.0:50051" or ":50051").
	grpcPort := s.cfg.GRPCAddr
	if _, port, err := net.SplitHostPort(s.cfg.GRPCAddr); err == nil {
		grpcPort = ":" + port
	}
	serverAddr := hostname + grpcPort // e.g. "myserver:50051"

	var lines []string
	lines = append(lines,
		"# DirQ Agent Configuration (generated by dirq-server)",
		"# Copy this file to /etc/dirq/agent.conf on each agent host.",
		"#",
		"# Then: sudo systemctl enable --now dirq-agent",
		"",
		"server: "+serverAddr,
		"listen: 0.0.0.0:50052",
		"exec_enabled: false",
	)

	if s.cfg.RegistrationSecret != "" {
		lines = append(lines, "registration_secret: "+s.cfg.RegistrationSecret)
	}

	// Embed TLS CA cert inline. Agent cert/key are NOT included — each agent
	// receives its own mTLS client cert during registration with the agent ID
	// as the CN. The CA cert is needed so the agent can verify the server.
	if tlsCfg.Enabled() && tlsCfg.CAFile != "" {
		if caData, err := os.ReadFile(tlsCfg.CAFile); err == nil {
			lines = append(lines, "", "# TLS CA certificate (base64-encoded PEM)")
			lines = append(lines, "tls_ca_data: "+base64.StdEncoding.EncodeToString(caData))
		}
	}

	// Embed server signing public key so agents can verify messages
	// from registration onward (trust-on-first-use is not needed).
	if s.signer != nil {
		lines = append(lines, "", "# Server signing public key (base64-encoded Ed25519)")
		lines = append(lines, "signing_public_key: "+base64.StdEncoding.EncodeToString(s.signer.PublicKey()))
	}

	lines = append(lines, "")
	content := strings.Join(lines, "\n")

	if err := os.WriteFile(outPath, []byte(content), 0600); err != nil {
		s.log.Warn("failed to write agent config", "path", outPath, "error", err)
		return
	}
	s.log.Info("agent config written — copy to /etc/dirq/agent.conf on each agent host", "path", outPath)
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
	s.tlsCfg = tlsCfg
	if tlsCfg.Enabled() {
		// Create a CertReloader for hot-reload support.
		reloader, err := tlsutil.NewCertReloader(tlsCfg.CertFile, tlsCfg.KeyFile)
		if err != nil {
			return fmt.Errorf("TLS cert reloader: %w", err)
		}
		s.certReloader = reloader

		// Use mixed-auth TLS: verify client certs if given, but don't
		// require them at the TLS layer. The mTLS interceptor enforces
		// per-method requirements (Register is exempt).
		creds, err := tlsutil.ServerCredentialsMixedAuthWithReloader(tlsCfg, reloader)
		if err != nil {
			return fmt.Errorf("TLS credentials: %w", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))

		// Enable mTLS enforcement if we have the CA key for issuing certs.
		if tlsCfg.CAKeyFile != "" {
			caCert, caKey, err := tlsutil.LoadCA(tlsCfg.CAFile, tlsCfg.CAKeyFile)
			if err != nil {
				return fmt.Errorf("load CA for mTLS: %w", err)
			}
			s.caCert = caCert
			s.caKey = caKey
			s.mtlsEnabled = true
			s.log.Info("mTLS enabled — agents receive client certs during registration")
		}
	} else {
		// Only reached when tls_disabled=true
	}

	// Add mTLS interceptors (they are no-ops when mtlsEnabled is false).
	grpcOpts = append(grpcOpts,
		grpc.UnaryInterceptor(s.mtlsUnaryInterceptor),
		grpc.StreamInterceptor(s.mtlsStreamInterceptor),
	)

	signer, err := signutil.EnsureServerSigner(signutil.ConfigFromEnv(s.cfg.FileCfg), s.log)
	if err != nil {
		return fmt.Errorf("signing setup: %w", err)
	}
	s.signer = signer

	// Load old signing key for rotation support.
	signCfg := signutil.ConfigFromEnv(s.cfg.FileCfg)
	if signCfg.OldPublicKeyFile != "" {
		oldPubData, err := os.ReadFile(signCfg.OldPublicKeyFile)
		if err != nil {
			s.log.Warn("failed to read old signing public key", "error", err)
		} else {
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(oldPubData)))
			if err != nil {
				s.log.Warn("failed to decode old signing public key", "error", err)
			} else {
				s.oldSignerPubKeys = append(s.oldSignerPubKeys, decoded)
				s.log.Info("loaded old signing public key for rotation")
			}
		}
	}

	// Write a ready-to-deploy agent config file with inline TLS certs
	// and the server's signing public key.
	s.writeAgentConfig(tlsCfg)

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
		s.httpSv.TLSConfig = &tls.Config{
			GetCertificate: s.certReloader.GetCertificate,
			MinVersion:     tls.VersionTLS12,
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

	// Leader election. When enabled, /readyz only returns 200 on the
	// pod that currently holds the singleton lock; standbys remain alive
	// (their /healthz passes) but stay out of the Service's endpoint set.
	if s.cfg.LeaderElection {
		s.leader = s.db.NewLeader(s.log)
		go s.leader.Run(ctx)
		s.log.Info("leader election enabled", "backend", s.db.Kind())
	}

	// Seed the in-memory topology from the DB so visibility (dirq hosts
	// graph) works during the few-second window before agents reconnect.
	// Routing itself is rebuilt from live streams; this is only for
	// operator observability across a restart.
	s.rehydrateTopology(ctx)

	// Start the stale-agent reaper, fact batcher, and topology snapshotter.
	// (The proactive topology rebalancer is gone — reactive paths
	// reassignOrphans + the registration batcher + orphan-promotion
	// fallback maintain the tree.)
	go s.startReaper(ctx)
	go s.runFactBatcher(ctx)
	go s.runTopologySnapshotter(ctx)

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

	// Periodic TLS cert reload.
	if s.certReloader != nil {
		go func() {
			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := s.certReloader.Reload(); err != nil {
						s.log.Warn("TLS cert reload failed", "error", err)
					}
				}
			}
		}()

		// SIGHUP triggers immediate cert reload.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGHUP)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-sigCh:
					s.log.Info("SIGHUP received, reloading TLS certificates")
					if err := s.certReloader.Reload(); err != nil {
						s.log.Error("TLS cert reload failed after SIGHUP", "error", err)
					}
				}
			}
		}()
	}

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
