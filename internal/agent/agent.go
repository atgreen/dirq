// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/host"
	"google.golang.org/grpc"
	grpccreds "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	grpcpeer "google.golang.org/grpc/peer"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/atgreen/dirq/internal/config"
	"github.com/atgreen/dirq/internal/modules"
	"github.com/atgreen/dirq/internal/query"
	"github.com/atgreen/dirq/internal/signutil"
	"github.com/atgreen/dirq/internal/tlsutil"
	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// Config holds agent configuration.
type Config struct {
	ServerAddr         string            // DirQ server address for bootstrap
	ListenAddr         string            // address this agent listens on for downstream peers
	Tags               map[string]string // user-defined tags
	Version            string
	ExecEnabled        bool         // Phase 2: whether this agent accepts exec/file requests
	RegistrationSecret string       // pre-shared secret for registration authentication
	FileCfg            *config.File // parsed config file (for TLS/signing fallback)

	// Hostname overrides os.Hostname() when reporting identity to the server
	// and when answering queries.  Used by emulation harnesses that run many
	// virtual hosts in one process.  Empty means use os.Hostname().
	Hostname string

	// InstanceName disambiguates per-agent state directories (e.g., mTLS cert
	// paths) when multiple agents share a process.  Empty preserves the legacy
	// single-tenant path under $DATA_DIR/tls/.
	InstanceName string

	// RegistrationJitter caps a random startup delay applied before the first
	// Register call, smoothing thundering-herd boot scenarios (multi-VH
	// emulation, rack-reboot recovery, post-maintenance fleet restart).
	// Zero disables jitter.
	RegistrationJitter time.Duration
}

// Agent is the DirQ endpoint agent.
type Agent struct {
	pb.UnimplementedDirQRelayServer

	cfg          Config
	log          *slog.Logger
	hostname     string // resolved at New(); used in registration and query results
	agentID      string
	role         pb.AgentRole
	sessionToken string   // from RegisterResponse, presented in AgentHello
	parentAddr    string   // where to connect upstream (server addr or parent's listen_addr)
	fallbackAddrs []string // backup parent addresses, tried before server on failure

	// gRPC server for downstream peers
	grpcSv *grpc.Server

	// Upstream connection (to server or parent peer)
	upstreamConn   *grpc.ClientConn
	upstreamStream pb.DirQServer_AgentStreamClient
	serverVerifier serverMessageVerifier

	// needsCertRenewal is set when a loaded mTLS cert is within 30 days of
	// expiry.  The agent still uses the cert (it's still valid) but will call
	// RenewCert shortly after the upstream stream is established.
	needsCertRenewal bool

	// tlsConfig is this agent's view of the TLS configuration.  Each agent in
	// a multi-tenant process holds its own copy so persistMTLSCert can repoint
	// CertFile/KeyFile without trampling its siblings.
	tlsConfig tlsutil.Config

	// Connected downstream peers
	mu          sync.RWMutex
	downstreams map[string]*downstreamPeer
}

type downstreamPeer struct {
	agentID string
	send    chan *pb.ServerMessage
	cancel  context.CancelFunc
}

// tlsBootstrapMu serializes tlsutil.EnsureCerts across in-process agents so
// concurrent first-run auto-generation doesn't race on the shared directory.
// Once files exist, EnsureCerts returns quickly and the mutex barely matters.
var tlsBootstrapMu sync.Mutex

// grpcDialOpts returns the standard gRPC dial options including TLS.
// When TLS is enabled, fails closed on credential-load errors rather than
// silently downgrading to plaintext.
func (a *Agent) grpcDialOpts() ([]grpc.DialOption, error) {
	opts := []grpc.DialOption{
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                60 * time.Second,
			Timeout:             15 * time.Second,
			PermitWithoutStream: true,
		}),
	}

	if a.tlsConfig.Enabled() {
		creds, err := tlsutil.ClientCredentials(a.tlsConfig)
		if err != nil {
			return nil, fmt.Errorf("load TLS credentials: %w", err)
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
		return opts, nil
	}
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	return opts, nil
}

// registrationDialOpts returns gRPC dial options for the Register RPC.
// Unlike grpcDialOpts, this does NOT present a client certificate — the agent
// doesn't have a server-issued cert yet. Only the CA is loaded so the agent
// can verify the server's identity.
func (a *Agent) registrationDialOpts() ([]grpc.DialOption, error) {
	opts := []grpc.DialOption{
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                60 * time.Second,
			Timeout:             15 * time.Second,
			PermitWithoutStream: true,
		}),
	}

	if a.tlsConfig.Enabled() {
		// Build a config with CA only — no client cert.
		regCfg := a.tlsConfig
		regCfg.CertFile = ""
		regCfg.KeyFile = ""
		creds, err := tlsutil.ClientCredentials(regCfg)
		if err != nil {
			return nil, fmt.Errorf("load TLS credentials: %w", err)
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
		return opts, nil
	}
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	return opts, nil
}

// peerDialOpts returns gRPC dial options for connecting to a peer agent's
// relay server.  Peer certs contain "localhost" as a SAN but not the peer's
// IP, so we override ServerName to "localhost" for TLS verification.
func (a *Agent) peerDialOpts() ([]grpc.DialOption, error) {
	opts := []grpc.DialOption{
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                60 * time.Second,
			Timeout:             15 * time.Second,
			PermitWithoutStream: true,
		}),
	}

	if a.tlsConfig.Enabled() {
		peerCfg := a.tlsConfig
		peerCfg.ServerName = "localhost"
		creds, err := tlsutil.ClientCredentials(peerCfg)
		if err != nil {
			return nil, fmt.Errorf("load TLS credentials: %w", err)
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
		return opts, nil
	}
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	return opts, nil
}

// New creates a new agent.
func New(cfg Config, log *slog.Logger) *Agent {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":50052"
	}
	hostname := cfg.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	return &Agent{
		cfg:         cfg,
		log:         log,
		hostname:    hostname,
		downstreams: make(map[string]*downstreamPeer),
	}
}

// Run starts the agent: registers with server, opens stream, listens for peers.
// If the upstream connection drops, Run reconnects with exponential backoff.
func (a *Agent) Run(ctx context.Context) error {
	// Step 0: Ensure TLS certs exist (auto-generate if needed).  Serialized
	// across in-process agents so concurrent EnsureCerts calls don't race on
	// the shared auto-gen directory.
	tlsBootstrapMu.Lock()
	tlsCfg := tlsutil.ConfigFromEnv(a.cfg.FileCfg)
	tlsCfg, err := tlsutil.EnsureCerts(tlsCfg, "agent", a.log)
	tlsBootstrapMu.Unlock()
	if err != nil {
		return fmt.Errorf("TLS setup: %w", err)
	}
	a.tlsConfig = tlsCfg

	// Load an existing mTLS cert from a previous registration (if valid).
	a.loadExistingMTLSCert()

	// Optional startup jitter: smooth boot stampedes (multi-VH emulation,
	// rack reboots, post-maintenance recovery).  Applied before bind so the
	// SO_REUSEADDR window also relaxes across siblings.
	if a.cfg.RegistrationJitter > 0 {
		delay := time.Duration(rand.Int64N(int64(a.cfg.RegistrationJitter)))
		a.log.Info("startup jitter", "delay", delay, "max", a.cfg.RegistrationJitter)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Step 1: Bind the relay listener before registering, so port collisions
	// (common in multi-VH emulation where 1000+ agents share a host) surface
	// as a Run() error instead of being logged in a background goroutine
	// while the agent stays registered but unreachable.
	lis, err := net.Listen("tcp", a.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("relay listen %s: %w", a.cfg.ListenAddr, err)
	}
	a.log.Info("relay server listening", "addr", a.cfg.ListenAddr)

	// Step 2: Register with the server (retry until success or context cancelled).
	if err := a.registerWithRetry(ctx); err != nil {
		lis.Close()
		return err
	}
	a.log.Info("registered", "agent_id", a.agentID, "role", a.role)

	// Step 3: Start serving downstream peers. Every agent listens — the
	// topology manager may assign children to any node at any time.
	go a.serveRelay(ctx, lis)

	// Step 4: Connect and run, reconnecting on failure.
	return a.connectLoop(ctx)
}

// registerWithRetry attempts registration with exponential backoff.
func (a *Agent) registerWithRetry(ctx context.Context) error {
	backoff := 1 * time.Second
	maxBackoff := 60 * time.Second

	for {
		err := a.register(ctx)
		if err == nil {
			return nil
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		a.log.Warn("registration failed, retrying", "error", err, "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = backoff * 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// connectLoop connects upstream and runs the main loop. If the connection
// drops, it reconnects with exponential backoff. Runs until ctx is cancelled.
func (a *Agent) connectLoop(ctx context.Context) error {
	backoff := 1 * time.Second
	maxBackoff := 60 * time.Second

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Try primary parent first.
		connected := false
		err := a.connectUpstream(ctx)
		if err == nil {
			connected = true
		} else {
			a.log.Warn("primary parent failed", "addr", a.parentAddr, "error", err)

			// Try fallback parents.
			for i, addr := range a.fallbackAddrs {
				a.log.Info("trying fallback parent", "fallback", i, "addr", addr)
				if err := a.connectToAddr(ctx, addr); err == nil {
					a.log.Info("connected to fallback parent", "fallback", i, "addr", addr)
					connected = true
					break
				}
				a.log.Warn("fallback parent failed", "fallback", i, "addr", addr, "error", err)
			}

			// Ask the server for a new parent assignment.
			if !connected {
				a.log.Info("all parents failed, requesting new assignment")
				if err := a.requestNewParent(ctx); err == nil {
					// Got a new parent — try connecting to it.
					if err := a.connectUpstream(ctx); err == nil {
						connected = true
					} else {
						a.log.Warn("new parent also unreachable", "addr", a.parentAddr, "error", err)
					}
				} else {
					a.log.Warn("request new parent failed", "error", err)
				}
			}
		}

		if !connected {
			// All connection attempts failed. The server may have restarted
			// (invalidating our session token). Re-register to get a fresh
			// token and topology assignment before retrying.
			a.log.Info("all connections failed, re-registering")
			if err := a.registerWithRetry(ctx); err != nil {
				return err
			}
			a.log.Info("re-registered", "agent_id", a.agentID, "role", a.role)

			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff = backoff * 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		// Reset backoff on successful connect.
		backoff = 1 * time.Second

		a.log.Info("upstream connected", "target", a.parentAddr)

		// If the cert was near expiry at load time, renew it now that we have
		// a live connection.  Failure is non-fatal: the existing cert is still
		// valid, and the next connect cycle will retry.
		if a.needsCertRenewal {
			if err := a.renewCert(ctx); err != nil {
				a.log.Warn("cert renewal failed, will retry later", "error", err)
			}
		}

		// Run the main loop until the stream breaks.
		err = a.mainLoop(ctx)

		// Clean up the old connection.
		if a.upstreamConn != nil {
			a.upstreamConn.Close()
			a.upstreamConn = nil
		}
		a.upstreamStream = nil

		if ctx.Err() != nil {
			return ctx.Err()
		}

		a.log.Warn("upstream connection lost, reconnecting", "error", err)
	}
}

func (a *Agent) register(ctx context.Context) error {
	dialOpts, err := a.registrationDialOpts()
	if err != nil {
		return fmt.Errorf("build TLS dial options: %w", err)
	}
	conn, err := grpc.NewClient(a.cfg.ServerAddr, dialOpts...)
	if err != nil {
		return fmt.Errorf("connect to server: %w", err)
	}
	defer conn.Close()

	client := pb.NewDirQServerClient(conn)

	hostname := a.hostname

	caps := []string{}
	for name := range modules.Registry() {
		caps = append(caps, name)
	}

	// Resolve the listen address to include our reachable IP.
	// If ListenAddr is just ":50052", we discover our outbound IP
	// by checking the connection to the server.
	listenAddr := a.cfg.ListenAddr
	if listenAddr != "" && listenAddr[0] == ':' {
		if outboundIP := resolveOutboundIP(a.cfg.ServerAddr); outboundIP != "" {
			listenAddr = outboundIP + listenAddr
		}
	}

	osName := runtime.GOOS
	var osVersion string
	if hi, err := host.Info(); err == nil {
		osVersion = hi.PlatformVersion
		// On Linux, Platform is the distro name (e.g. "fedora", "rhel").
		// On Windows, Platform is a long string like "Microsoft Windows Server
		// 2022 Standard" which breaks os == "windows" checks, so we keep
		// runtime.GOOS for Windows.
		if runtime.GOOS != "windows" && hi.Platform != "" {
			osName = hi.Platform
		}
	}

	resp, err := client.Register(ctx, &pb.RegisterRequest{
		Hostname:           hostname,
		Os:                 osName,
		OsVersion:          osVersion,
		Arch:               runtime.GOARCH,
		AgentVersion:       a.cfg.Version,
		Capabilities:       caps,
		ListenAddr:         listenAddr,
		Tags:               a.cfg.Tags,
		ExecEnabled:        a.cfg.ExecEnabled,
		RegistrationSecret: a.cfg.RegistrationSecret,
	})
	if err != nil {
		return fmt.Errorf("register RPC: %w", err)
	}

	a.agentID = resp.AgentId
	a.role = resp.Role
	a.sessionToken = resp.SessionToken
	if err := a.setServerVerifier(resp.GetServerSigningPublicKey(), resp.GetServerSigningKeyId(), resp.GetServerSigningPublicKeysOld()); err != nil {
		return fmt.Errorf("load server signing key: %w", err)
	}

	// Persist server-issued mTLS client certificate if provided.
	if len(resp.TlsClientCert) > 0 && len(resp.TlsClientKey) > 0 {
		if err := a.persistMTLSCert(resp.TlsClientCert, resp.TlsClientKey, resp.TlsCaCert); err != nil {
			a.log.Error("failed to persist mTLS cert", "error", err)
			// Non-fatal: agent can still operate without mTLS.
		} else {
			a.log.Info("mTLS client cert persisted", "agent_id", resp.AgentId)
		}
	}

	// Determine where to connect upstream.
	if resp.ZoneLeaderAddr != "" && resp.Role != pb.AgentRole_AGENT_ROLE_ZONE_LEADER {
		a.parentAddr = resp.ZoneLeaderAddr
		a.fallbackAddrs = resp.FallbackAddrs
		a.log.Info("assigned to parent",
			"parent_addr", a.parentAddr,
			"role", resp.Role,
			"fallbacks", len(a.fallbackAddrs),
		)
	} else {
		a.parentAddr = a.cfg.ServerAddr
		a.fallbackAddrs = nil
	}

	return nil
}

func (a *Agent) connectUpstream(ctx context.Context) error {
	target := a.parentAddr
	if target == "" {
		target = a.cfg.ServerAddr
	}

	a.log.Info("connecting upstream", "target", target, "role", a.role)

	// Zone leaders connect to the server's AgentStream RPC.
	// Relays and leafs connect to their parent's RelayStream RPC.
	if a.role == pb.AgentRole_AGENT_ROLE_ZONE_LEADER {
		dialOpts, err := a.grpcDialOpts()
		if err != nil {
			return fmt.Errorf("build TLS dial options: %w", err)
		}
		conn, err := grpc.NewClient(target, dialOpts...)
		if err != nil {
			return fmt.Errorf("connect upstream: %w", err)
		}
		a.upstreamConn = conn

		client := pb.NewDirQServerClient(conn)
		stream, err := client.AgentStream(ctx)
		if err != nil {
			return fmt.Errorf("open agent stream: %w", err)
		}
		a.upstreamStream = stream
	} else {
		// Use peer dial options (ServerName override) for relay connections.
		dialOpts, err := a.peerDialOpts()
		if err != nil {
			return fmt.Errorf("build TLS dial options: %w", err)
		}
		conn, err := grpc.NewClient(target, dialOpts...)
		if err != nil {
			return fmt.Errorf("connect upstream: %w", err)
		}
		a.upstreamConn = conn

		client := pb.NewDirQRelayClient(conn)
		stream, err := client.RelayStream(ctx)
		if err != nil {
			conn.Close()
			a.upstreamConn = nil
			return fmt.Errorf("relay stream to %s: %w", target, err)
		}
		a.upstreamStream = stream
	}

	return a.sendHello()
}

// connectToAddr connects to a specific address as a relay peer (for fallbacks).
func (a *Agent) connectToAddr(ctx context.Context, addr string) error {
	dialOpts, err := a.peerDialOpts()
	if err != nil {
		return fmt.Errorf("build TLS dial options: %w", err)
	}
	conn, err := grpc.NewClient(addr, dialOpts...)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}

	client := pb.NewDirQRelayClient(conn)
	stream, err := client.RelayStream(ctx)
	if err != nil {
		conn.Close()
		return fmt.Errorf("relay stream to %s: %w", addr, err)
	}

	a.upstreamConn = conn
	a.upstreamStream = stream

	if err := a.sendHello(); err != nil {
		conn.Close()
		return fmt.Errorf("hello to %s: %w", addr, err)
	}

	return nil
}

// sendHello sends an AgentHello on the upstream stream. Must be the first
// message on any new stream — the server/relay rejects streams without it.
func (a *Agent) sendHello() error {
	caps := []string{}
	for name := range modules.Registry() {
		caps = append(caps, name)
	}
	return a.upstreamStream.Send(&pb.AgentMessage{
		Payload: &pb.AgentMessage_Hello{
			Hello: &pb.AgentHello{
				AgentId:      a.agentID,
				Capabilities: caps,
				ExecEnabled:  a.cfg.ExecEnabled,
				SessionToken: a.sessionToken,
			},
		},
	})
}

// mtlsCertDir returns the directory for this agent's mTLS client certs.
// When InstanceName is set, certs live under tls/instances/<name>/ so multiple
// agents in the same process don't trample each other's files.
func (a *Agent) mtlsCertDir() string {
	if a.cfg.InstanceName != "" {
		return filepath.Join(config.DataDir(), "tls", "instances", a.cfg.InstanceName)
	}
	return filepath.Join(config.DataDir(), "tls")
}

// persistMTLSCert saves the server-issued client cert/key/CA to disk and
// updates the agent's tlsConfig so subsequent connections use mTLS.
func (a *Agent) persistMTLSCert(certPEM, keyPEM, caCertPEM []byte) error {
	dir := a.mtlsCertDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create mTLS cert dir: %w", err)
	}

	certPath := filepath.Join(dir, "mtls-client.crt")
	keyPath := filepath.Join(dir, "mtls-client.key")

	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return fmt.Errorf("write client cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return fmt.Errorf("write client key: %w", err)
	}

	// Also persist CA cert if provided and not already present.
	if len(caCertPEM) > 0 {
		caPath := filepath.Join(dir, "ca.crt")
		if err := os.WriteFile(caPath, caCertPEM, 0644); err != nil {
			return fmt.Errorf("write CA cert: %w", err)
		}
	}

	// Update TLS config to use the new cert for all subsequent connections.
	a.tlsConfig.CertFile = certPath
	a.tlsConfig.KeyFile = keyPath
	if len(caCertPEM) > 0 {
		a.tlsConfig.CAFile = filepath.Join(dir, "ca.crt")
		a.tlsConfig.Insecure = false
	}

	return nil
}

// renewCert calls the server's RenewCert RPC to obtain a fresh mTLS client
// certificate without a full re-registration.  It persists the new cert to
// disk and updates the agent's tlsConfig.
func (a *Agent) renewCert(ctx context.Context) error {
	dialOpts, err := a.grpcDialOpts()
	if err != nil {
		return fmt.Errorf("build TLS dial options: %w", err)
	}
	conn, err := grpc.NewClient(a.cfg.ServerAddr, dialOpts...)
	if err != nil {
		return fmt.Errorf("connect to server for cert renewal: %w", err)
	}
	defer conn.Close()

	client := pb.NewDirQServerClient(conn)
	resp, err := client.RenewCert(ctx, &pb.RenewCertRequest{AgentId: a.agentID})
	if err != nil {
		return fmt.Errorf("RenewCert RPC: %w", err)
	}

	if len(resp.TlsClientCert) == 0 || len(resp.TlsClientKey) == 0 {
		return fmt.Errorf("RenewCert returned empty cert or key")
	}

	if err := a.persistMTLSCert(resp.TlsClientCert, resp.TlsClientKey, resp.TlsCaCert); err != nil {
		return fmt.Errorf("persist renewed mTLS cert: %w", err)
	}

	a.needsCertRenewal = false
	a.log.Info("mTLS client cert renewed successfully", "agent_id", a.agentID)
	return nil
}

// loadExistingMTLSCert checks for a previously-issued mTLS cert and loads
// it into the TLS config if valid.
func (a *Agent) loadExistingMTLSCert() {
	dir := a.mtlsCertDir()
	certPath := filepath.Join(dir, "mtls-client.crt")
	keyPath := filepath.Join(dir, "mtls-client.key")

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return // no existing cert
	}
	if _, err := os.Stat(keyPath); err != nil {
		return // no key
	}

	// Check expiry.  If the cert is within 30 days of expiry, still load it
	// (it remains valid) but schedule a renewal via RenewCert after connecting.
	if tlsutil.CertExpiresWithin(certPEM, 30*24*time.Hour) {
		a.log.Info("existing mTLS cert expires soon, will renew after connecting")
		a.needsCertRenewal = true
		// Fall through — continue loading the cert so we can use it right now.
	}

	// Verify the CN matches our expected agent ID (if we have one from
	// a previous run). On first run, agentID is empty so we accept any cert.
	if a.agentID != "" {
		cn := tlsutil.CertCN(certPEM)
		if cn != a.agentID {
			a.log.Info("existing mTLS cert CN mismatch, will re-register",
				"cert_cn", cn, "agent_id", a.agentID)
			return
		}
	}

	// Load it.
	a.tlsConfig.CertFile = certPath
	a.tlsConfig.KeyFile = keyPath
	caPath := filepath.Join(dir, "ca.crt")
	if _, err := os.Stat(caPath); err == nil {
		a.tlsConfig.CAFile = caPath
		a.tlsConfig.Insecure = false
	}
	a.log.Info("loaded existing mTLS client cert", "cert", certPath)
}

// requestNewParent calls the server's RequestPeers RPC to get a new parent
// assignment.  Updates parentAddr and fallbackAddrs on success.  If the
// server can't find a parent with room it may promote this agent to zone
// leader instead, in which case we reconnect directly to the server.
func (a *Agent) requestNewParent(ctx context.Context) error {
	dialOpts, err := a.grpcDialOpts()
	if err != nil {
		return fmt.Errorf("build TLS dial options: %w", err)
	}
	conn, err := grpc.NewClient(a.cfg.ServerAddr, dialOpts...)
	if err != nil {
		return fmt.Errorf("connect to server: %w", err)
	}
	defer conn.Close()

	client := pb.NewDirQServerClient(conn)
	resp, err := client.RequestPeers(ctx, &pb.PeerRequest{AgentId: a.agentID})
	if err != nil {
		return fmt.Errorf("RequestPeers RPC: %w", err)
	}

	// Server promoted us to zone leader (tree saturated, no parent has room).
	// Reconnect directly to the server via AgentStream.
	if resp.NewRole == pb.AgentRole_AGENT_ROLE_ZONE_LEADER {
		a.role = pb.AgentRole_AGENT_ROLE_ZONE_LEADER
		a.parentAddr = a.cfg.ServerAddr
		a.fallbackAddrs = nil
		a.log.Info("server promoted us to zone_leader; reconnecting to server")
		return nil
	}

	if resp.ZoneLeaderAddr == "" {
		return fmt.Errorf("server returned no parent assignment")
	}

	a.parentAddr = resp.ZoneLeaderAddr
	a.fallbackAddrs = resp.FallbackAddrs
	a.log.Info("server assigned new parent",
		"parent_addr", a.parentAddr,
		"fallbacks", len(a.fallbackAddrs),
	)
	return nil
}

func (a *Agent) mainLoop(ctx context.Context) error {
	// Start receiving from upstream in a goroutine.
	msgCh := make(chan *pb.ServerMessage, 16)
	errCh := make(chan error, 1)
	go func() {
		for {
			msg, err := a.upstreamStream.Recv()
			if err == io.EOF {
				errCh <- nil
				return
			}
			if err != nil {
				errCh <- err
				return
			}
			msgCh <- msg
		}
	}()

	// Periodic cert check — every 12 hours while the stream is live.
	certCheckTicker := time.NewTicker(12 * time.Hour)
	defer certCheckTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-errCh:
			return fmt.Errorf("upstream stream error: %w", err)

		case msg := <-msgCh:
			a.handleServerMessage(ctx, msg)

		case <-certCheckTicker.C:
			// Re-read the cert from disk to check its current expiry.
			dir := a.mtlsCertDir()
			certPEM, err := os.ReadFile(filepath.Join(dir, "mtls-client.crt"))
			if err == nil && tlsutil.CertExpiresWithin(certPEM, 30*24*time.Hour) {
				a.log.Info("periodic cert check: cert expires soon, renewing")
				if err := a.renewCert(ctx); err != nil {
					a.log.Warn("periodic cert renewal failed", "error", err)
				}
			}
		}
	}
}

// notifyPeerDisconnected sends a PeerDisconnected message upstream so the
// server can immediately mark the agent (and its subtree) offline.
func (a *Agent) notifyPeerDisconnected(peerID string) {
	if a.upstreamStream == nil {
		return
	}
	err := a.upstreamStream.Send(&pb.AgentMessage{
		Payload: &pb.AgentMessage_PeerDisconnected{
			PeerDisconnected: &pb.PeerDisconnected{
				AgentId: peerID,
			},
		},
	})
	if err != nil {
		a.log.Error("failed to send peer disconnected", "peer_id", peerID, "error", err)
	}
}

// notifyPeerConnected sends a PeerConnected message upstream so the
// server can mark the agent back online and commit its new parent
// assignment after a reattachment.  Without this signal, an agent that
// reattaches via a fallback parent would stay erroneously offline in
// server topology — there is no other upstream notification of the new
// attachment outside of full re-registration.
func (a *Agent) notifyPeerConnected(peerID string) {
	if a.upstreamStream == nil {
		return
	}
	err := a.upstreamStream.Send(&pb.AgentMessage{
		Payload: &pb.AgentMessage_PeerConnected{
			PeerConnected: &pb.PeerConnected{
				AgentId:  peerID,
				ParentId: a.agentID,
			},
		},
	})
	if err != nil {
		a.log.Error("failed to send peer connected", "peer_id", peerID, "error", err)
	}
}

func (a *Agent) handleServerMessage(ctx context.Context, msg *pb.ServerMessage) {
	if err := a.verifyServerMessage(msg); err != nil {
		a.log.Error("rejected unsigned or invalid server message", "error", err)
		return
	}

	switch p := msg.Payload.(type) {
	case *pb.ServerMessage_QueryRequest:
		// Queries are broadcast — relay the original signed message to downstream
		// peers, then execute locally.
		a.relayToDownstreams(msg)
		go a.executeQuery(ctx, p.QueryRequest)
	case *pb.ServerMessage_PeerUpdate:
		pu := p.PeerUpdate
		// If the update targets a specific agent and it's not us, relay downstream.
		if pu.TargetAgentId != "" && pu.TargetAgentId != a.agentID {
			a.relayToDownstreams(msg)
			return
		}
		if pu.NewRole == pb.AgentRole_AGENT_ROLE_ZONE_LEADER && pu.NewParentAddr == "" {
			// Promotion to zone leader — reconnect directly to the server.
			// Our children stay connected to us — zero disruption for them.
			a.log.Info("rebalance: promoted to zone leader, reconnecting to server",
				"previous_role", a.role,
			)
			a.parentAddr = a.cfg.ServerAddr
			a.fallbackAddrs = nil
			a.role = pb.AgentRole_AGENT_ROLE_ZONE_LEADER
			if a.upstreamConn != nil {
				a.upstreamConn.Close()
			}
			return
		}
		if pu.NewParentAddr != "" {
			// Reassignment to a new parent (demotion or rebalance).
			a.log.Info("rebalance: reconnecting to new parent",
				"new_parent", pu.NewParentAddr,
				"new_role", pu.NewRole,
			)
			a.parentAddr = pu.NewParentAddr
			a.fallbackAddrs = pu.NewFallbackAddrs
			if pu.NewRole != pb.AgentRole_AGENT_ROLE_UNSPECIFIED {
				a.role = pu.NewRole
			}
			if a.upstreamConn != nil {
				a.upstreamConn.Close()
			}
			return
		}
		a.log.Info("peer update received", "new_role", pu.NewRole)
	case *pb.ServerMessage_UpdatePush:
		a.log.Info("update push received", "version", p.UpdatePush.Version)
	case *pb.ServerMessage_ExecRequest:
		if len(p.ExecRequest.TargetAgentIds) > 0 {
			// Broadcast mode — relay first, then execute if targeted.
			a.relayToDownstreams(msg)
			if a.isTargeted(p.ExecRequest.TargetAgentIds) {
				go a.handleExecRequest(ctx, p.ExecRequest)
			}
		} else if p.ExecRequest.AgentId == a.agentID {
			// Point-to-point mode — execute locally.
			go a.handleExecRequest(ctx, p.ExecRequest)
		} else {
			// Point-to-point mode — relay downstream toward target.
			a.relayToDownstreams(msg)
		}
	case *pb.ServerMessage_PutFile:
		if p.PutFile.AgentId == a.agentID {
			go a.handlePutFile(ctx, p.PutFile)
		} else {
			a.relayToDownstreams(msg)
		}
	case *pb.ServerMessage_FetchFile:
		if p.FetchFile.AgentId == a.agentID {
			go a.handleFetchFile(ctx, p.FetchFile)
		} else {
			a.relayToDownstreams(msg)
		}
	case *pb.ServerMessage_DeployRequest:
		// Deploy is broadcast — relay first (like queries), then execute if targeted.
		a.relayToDownstreams(msg)
		if a.isTargeted(p.DeployRequest.TargetAgentIds) {
			go a.handleDeploy(ctx, p.DeployRequest)
		}
	case *pb.ServerMessage_RotateCommand:
		rc := p.RotateCommand
		a.log.Info("rotation command received",
			"type", rc.GetType().String(),
			"reason", rc.GetReason(),
		)
		// Relay to downstream peers first so the rotation propagates through
		// the mesh before we act on it locally.
		a.relayToDownstreams(msg)
		go a.handleRotateCommand(ctx, rc)
	}
}

func (a *Agent) handleRotateCommand(ctx context.Context, rc *pb.RotateCommand) {
	// Stagger: wait a random delay to avoid thundering herd on RenewCert.
	if s := rc.GetStaggerSeconds(); s > 0 {
		delay := time.Duration(rand.IntN(int(s))) * time.Second
		a.log.Info("staggering rotation", "delay", delay, "type", rc.GetType().String())
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
	}

	switch rc.GetType() {
	case pb.RotateCommand_ROTATION_TYPE_AGENT_CERT:
		a.log.Info("rotating agent cert via RenewCert")
		if err := a.renewCert(ctx); err != nil {
			a.log.Error("cert rotation failed", "error", err)
		}
	case pb.RotateCommand_ROTATION_TYPE_SIGNING_KEY:
		newKey := rc.GetNewSigningPublicKey()
		newKeyID := rc.GetNewSigningKeyId()
		if len(newKey) == 0 {
			a.log.Error("signing key rotation: no new public key provided")
			return
		}
		newVerifier, err := signutil.NewVerifier(newKey, newKeyID)
		if err != nil {
			a.log.Error("signing key rotation: failed to build new verifier", "error", err)
			return
		}
		// Build a MultiVerifier that trusts both the new key and the current
		// verifier (for in-flight messages signed with the old key).
		var allVerifiers []*signutil.Verifier
		allVerifiers = append(allVerifiers, newVerifier)
		if cur, ok := a.serverVerifier.(*signutil.Verifier); ok {
			allVerifiers = append(allVerifiers, cur)
		} else if curMulti, ok := a.serverVerifier.(*signutil.MultiVerifier); ok {
			// Already a multi-verifier — keep all existing trusted keys.
			allVerifiers = append(allVerifiers, curMulti.Verifiers()...)
		}
		multi := signutil.NewMultiVerifier(allVerifiers...)
		a.serverVerifier = multi
		a.log.Info("signing key rotated", "new_key_id", newKeyID)
	case pb.RotateCommand_ROTATION_TYPE_CA:
		a.log.Info("rotating CA bundle via RenewCert")
		if err := a.renewCert(ctx); err != nil {
			a.log.Error("CA rotation failed", "error", err)
		}
	default:
		a.log.Warn("unknown rotation type, ignoring", "type", rc.GetType())
	}
}

func (a *Agent) executeQuery(ctx context.Context, qr *pb.QueryRequest) {
	// Relay is handled by handleServerMessage before this is called.

	// Check if this agent is a target. If target_agent_ids is empty, it's a
	// broadcast (all agents execute). If populated, only listed agents execute.
	if len(qr.TargetAgentIds) > 0 && !a.isTargeted(qr.TargetAgentIds) {
		return // not a target — just relay, don't execute
	}

	a.log.Info("executing query", "query_id", qr.QueryId, "modules", qr.Modules)

	hostname := a.hostname

	// Extract name hints from filters for optimized collection.
	// e.g., packages.name = 'kernel' → hints{"packages": ["kernel"]}
	hints := modules.ModuleHints{}
	for _, f := range qr.Filters {
		parts := strings.SplitN(f.Field, ".", 2)
		if len(parts) == 2 && parts[1] == "name" && (f.Operator == "=" || f.Operator == "IN") {
			if f.Operator == "=" {
				hints[parts[0]] = append(hints[parts[0]], f.Value)
			} else {
				// IN operator: value is comma-separated.
				for _, v := range strings.Split(f.Value, ",") {
					v = strings.TrimSpace(v)
					if v != "" {
						hints[parts[0]] = append(hints[parts[0]], v)
					}
				}
			}
		}
	}

	// Collect data from requested modules.
	collected := modules.CollectModules(qr.Modules, hints)

	// Inject top-level "hostname" so WHERE hostname = 'foo' works
	// without requiring the os_info. prefix.
	collected["hostname"] = hostname

	// Overlay our hostname onto os_info.hostname.  The module collects it via
	// os.Hostname() which returns the physical host; virtual hosts sharing a
	// process need to report their synthesized identity here too.
	if osInfo, ok := collected["os_info"].(map[string]any); ok {
		osInfo["hostname"] = hostname
	}

	// Apply agent-side filtering (array-aware: filters into packages, services, etc.)
	if len(qr.Filters) > 0 {
		conds := protoFiltersToConditions(qr.Filters)
		collected = query.FilterCollectedData(conds, collected)

		// Check if all WHERE-referenced modules survived filtering.
		// If a module was referenced in a condition but is missing from the
		// result, this agent didn't match — send a no-match response so the
		// server can count completions without waiting for an idle timeout.
		if !query.AllFilteredModulesPresent(conds, collected) {
			a.sendQueryResult(qr.QueryId, hostname, false, "no match", nil)
			return
		}
	}

	data, err := structpb.NewStruct(collected)
	if err != nil {
		a.log.Error("failed to create struct", "error", err)
		a.sendQueryResult(qr.QueryId, hostname, false, err.Error(), nil)
		return
	}

	a.sendQueryResult(qr.QueryId, hostname, true, "", data)
}

// isTargeted checks if this agent's ID is in the target list.
func (a *Agent) isTargeted(targetIDs []string) bool {
	for _, id := range targetIDs {
		if id == a.agentID {
			return true
		}
	}
	return false
}

func (a *Agent) sendQueryResult(queryID, hostname string, success bool, errMsg string, data *structpb.Struct) {
	msg := &pb.AgentMessage{
		Payload: &pb.AgentMessage_QueryResult{
			QueryResult: &pb.QueryResult{
				QueryId:     queryID,
				AgentId:     a.agentID,
				Hostname:    hostname,
				Success:     success,
				Error:       errMsg,
				Data:        data,
				CollectedAt: timestamppb.Now(),
			},
		},
	}
	if err := a.upstreamStream.Send(msg); err != nil {
		a.log.Error("failed to send query result", "error", err)
	}
}

// relayToDownstreams sends any ServerMessage to all connected downstream peers.
func (a *Agent) relayToDownstreams(msg *pb.ServerMessage) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	for _, ds := range a.downstreams {
		select {
		case ds.send <- msg:
		default:
			a.log.Warn("downstream send buffer full", "peer", ds.agentID)
		}
	}
}

// ─────────────────────────────────────────────────────────
// Relay server (for downstream peers)
// ─────────────────────────────────────────────────────────

// serveRelay runs the downstream gRPC server on the pre-bound listener. The
// listener is bound synchronously in Run() so port-bind failures surface as
// Run() errors.
func (a *Agent) serveRelay(ctx context.Context, lis net.Listener) {
	var serverOpts []grpc.ServerOption
	if a.tlsConfig.Enabled() {
		creds, err := tlsutil.ServerCredentials(a.tlsConfig)
		if err != nil {
			a.log.Error("relay TLS setup failed, using insecure", "error", err)
		} else {
			serverOpts = append(serverOpts, grpc.Creds(creds))
		}
	}
	// Add keepalive policy to allow client pings without ENHANCE_YOUR_CALM.
	serverOpts = append(serverOpts,
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             30 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	a.grpcSv = grpc.NewServer(serverOpts...)
	pb.RegisterDirQRelayServer(a.grpcSv, a)

	go func() {
		<-ctx.Done()
		a.grpcSv.GracefulStop()
	}()

	if err := a.grpcSv.Serve(lis); err != nil {
		a.log.Error("relay server error", "error", err)
	}
}

// RelayStream handles a downstream peer connecting to this agent.
func (a *Agent) RelayStream(stream pb.DirQRelay_RelayStreamServer) error {
	// First message must be a Hello.
	msg, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := msg.GetHello()
	if hello == nil {
		return fmt.Errorf("first message must be AgentHello")
	}

	peerID := hello.AgentId

	// If the peer presented a TLS client certificate, verify that its CN
	// matches the claimed agent ID.
	if cn := peerTLSCN(stream.Context()); cn != "" && cn != peerID {
		a.log.Warn("downstream peer rejected: cert CN mismatch",
			"cert_cn", cn, "claimed_agent_id", peerID)
		return fmt.Errorf("cert CN %q does not match agent_id %q", cn, peerID)
	}

	// Verify the session token: it's the server's Ed25519 signature over
	// the agent ID. We can verify it using the signing public key we
	// received during registration.
	if hello.SessionToken == "" {
		a.log.Warn("downstream peer rejected: no session token", "peer_id", peerID)
		return fmt.Errorf("peer %s provided no session token", peerID)
	}
	if a.serverVerifier != nil && !a.serverVerifier.VerifyToken(peerID, hello.SessionToken) {
		a.log.Warn("downstream peer rejected: invalid session token", "peer_id", peerID)
		return fmt.Errorf("invalid session token for peer %s", peerID)
	}

	a.log.Info("downstream peer connected", "peer_id", peerID)

	// Notify upstream so the server commits the new attachment and
	// flips the peer back to online if it was in a detached state
	// (e.g. left over from a ZL failover that marked the subtree
	// offline pending proof of reattachment).
	a.notifyPeerConnected(peerID)

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	ds := &downstreamPeer{
		agentID: peerID,
		send:    make(chan *pb.ServerMessage, 64),
		cancel:  cancel,
	}

	a.mu.Lock()
	a.downstreams[peerID] = ds
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.downstreams, peerID)
		a.mu.Unlock()
		a.log.Info("downstream peer disconnected", "peer_id", peerID)
		a.notifyPeerDisconnected(peerID)
	}()

	// Sender goroutine
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-ds.send:
				if err := stream.Send(msg); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Receiver loop — route downstream responses appropriately.
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		// Forward everything upstream immediately — no buffering.
		if err := a.upstreamStream.Send(msg); err != nil {
			a.log.Error("failed to relay upstream", "peer_id", peerID, "error", err)
			return err
		}
	}
}

// ─────────────────────────────────────────────────────────
// Agent-side filtering
// ─────────────────────────────────────────────────────────

// protoFiltersToConditions converts proto Filter messages back into query.Condition
// objects so the agent can use the query package's FilterCollectedData.
// peerTLSCN extracts the CN from a gRPC peer's TLS client certificate.
// Returns empty string if no client cert is present.
func peerTLSCN(ctx context.Context) string {
	p, ok := grpcpeer.FromContext(ctx)
	if !ok {
		return ""
	}
	tlsInfo, ok := p.AuthInfo.(grpccreds.TLSInfo)
	if !ok {
		return ""
	}
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		// No verified client cert — peer connected without mTLS.
		// Also check PeerCertificates for the case where VerifyClientCertIfGiven
		// is used and the cert was presented but chains weren't built.
		if len(tlsInfo.State.PeerCertificates) == 0 {
			return ""
		}
		return tlsInfo.State.PeerCertificates[0].Subject.CommonName
	}
	return tlsInfo.State.VerifiedChains[0][0].Subject.CommonName
}

// resolveOutboundIP discovers the IP address this host uses to reach a target.
// It opens a UDP connection (no actual traffic) to the target and reads the
// local address. Returns empty string on failure.
func resolveOutboundIP(target string) string {
	// Strip port if target has one, use a dummy port for UDP dial.
	host := target
	if h, _, err := net.SplitHostPort(target); err == nil {
		host = h
	}
	conn, err := net.Dial("udp", host+":1")
	if err != nil {
		return ""
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}

func protoFiltersToConditions(filters []*pb.Filter) []*query.Condition {
	conds := make([]*query.Condition, 0, len(filters))
	for _, f := range filters {
		if f.Operator == "IN" {
			// IN operator: value is comma-separated list.
			values := strings.Split(f.Value, ",")
			conds = append(conds, &query.Condition{
				Field: f.Field,
				In:    &query.InClause{Values: values},
			})
		} else {
			c := &query.Condition{
				Field:    f.Field,
				Operator: f.Operator,
			}
			// Try to parse as number, fall back to string.
			if num, err := strconv.ParseFloat(f.Value, 64); err == nil {
				c.Value = &query.Value{Number: &num}
			} else {
				val := f.Value
				c.Value = &query.Value{String: &val}
			}
			conds = append(conds, c)
		}
	}
	return conds
}
