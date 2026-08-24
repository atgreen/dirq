// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

// Package tlsutil provides TLS configuration helpers for DirQ server and agents.
//
// TLS is enabled by default. If no cert/key files are configured, self-signed
// certificates are auto-generated at startup. To explicitly disable TLS (not
// recommended), set DIRQ_TLS_DISABLED=true.
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/atgreen/dirq/internal/config"
	"google.golang.org/grpc/credentials"
)

// Config holds TLS file paths loaded from environment variables.
type Config struct {
	CAFile     string // path to CA certificate
	CAFileOld  string // path to old CA certificate (for CA rotation)
	CAKeyFile  string // path to CA private key (needed for mTLS cert issuance)
	CertFile   string // path to this process's certificate
	KeyFile    string // path to this process's private key
	Insecure   bool   // skip cert verification (for self-signed)
	Disabled   bool   // explicitly disable TLS (DIRQ_TLS_DISABLED=true)
	ServerName string // override TLS ServerName for verification (peer connections)
	// ExpectedPeerCN, when set, pins the identity of the server side of the
	// connection: after the normal CA-chain + hostname check passes, the peer
	// leaf certificate's CommonName must equal this value. Used for agent→parent
	// (relay) connections so a child authenticates the *specific* parent the
	// server assigned it, not merely any holder of a CA-issued agent cert.
	ExpectedPeerCN string
}

// Enabled returns true if TLS should be used. TLS is on by default —
// it's only off when explicitly disabled.
func (c Config) Enabled() bool {
	return !c.Disabled
}

// HasUserCerts returns true if the user provided their own cert and key.
func (c Config) HasUserCerts() bool {
	return c.CertFile != "" && c.KeyFile != ""
}

// ConfigFromEnv reads TLS configuration from environment variables,
// falling back to config file values.
//
// Supports inline base64-encoded PEM via tls_ca_data, tls_cert_data,
// tls_key_data keys. When present and no file path is set, the PEM
// content is decoded and written to the data directory so everything
// can be distributed in a single config file.
func ConfigFromEnv(fileCfg ...*config.File) Config {
	var fc *config.File
	if len(fileCfg) > 0 {
		fc = fileCfg[0]
	}
	cfg := Config{
		CAFile:    config.EnvOr("DIRQ_TLS_CA", fc, "tls_ca", ""),
		CAFileOld: config.EnvOr("DIRQ_TLS_CA_OLD", fc, "tls_ca_old", ""),
		CAKeyFile: config.EnvOr("DIRQ_TLS_CA_KEY", fc, "tls_ca_key", ""),
		CertFile:  config.EnvOr("DIRQ_TLS_CERT", fc, "tls_cert", ""),
		KeyFile:   config.EnvOr("DIRQ_TLS_KEY", fc, "tls_key", ""),
		Insecure:  config.EnvOr("DIRQ_TLS_INSECURE", fc, "tls_insecure", "false") == "true",
		Disabled:  config.EnvOr("DIRQ_TLS_DISABLED", fc, "tls_disabled", "false") == "true",
	}

	// Materialize inline cert data to files if no paths were given.
	dir := filepath.Join(config.DataDir(), "tls")
	os.MkdirAll(dir, 0700)

	if cfg.CAFile == "" {
		if data := config.EnvOr("DIRQ_TLS_CA_DATA", fc, "tls_ca_data", ""); data != "" {
			if p, err := materializePEM(dir, "ca.crt", data); err == nil {
				cfg.CAFile = p
			}
		}
	}
	if cfg.CertFile == "" {
		if data := config.EnvOr("DIRQ_TLS_CERT_DATA", fc, "tls_cert_data", ""); data != "" {
			if p, err := materializePEM(dir, "agent.crt", data); err == nil {
				cfg.CertFile = p
			}
		}
	}
	if cfg.KeyFile == "" {
		if data := config.EnvOr("DIRQ_TLS_KEY_DATA", fc, "tls_key_data", ""); data != "" {
			if p, err := materializePEM(dir, "agent.key", data); err == nil {
				cfg.KeyFile = p
			}
		}
	}

	return cfg
}

// materializePEM decodes base64-encoded PEM data and writes it to a file.
func materializePEM(dir, name, b64data string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(b64data)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, decoded, 0600); err != nil {
		return "", err
	}
	return path, nil
}

// autoGenDir returns the directory for auto-generated certs.
func autoGenDir() string {
	return filepath.Join(config.DataDir(), "tls")
}

// EnsureCerts makes sure TLS cert/key files exist. If the user provided their
// own, those are used. Otherwise, self-signed certs are auto-generated.
// Returns the (possibly updated) Config and logs what happened.
func EnsureCerts(cfg Config, role string, log *slog.Logger) (Config, error) {
	if cfg.Disabled {
		log.Warn("TLS explicitly disabled — all gRPC connections are unencrypted. NOT RECOMMENDED for production.")
		return cfg, nil
	}

	if cfg.HasUserCerts() {
		if cfg.CAFile != "" {
			log.Info("TLS enabled with user-supplied certs (mTLS)",
				"cert", cfg.CertFile, "ca", cfg.CAFile)
		} else {
			log.Info("TLS enabled with user-supplied certs",
				"cert", cfg.CertFile)
		}
		return cfg, nil
	}

	// If the agent has a CA but no cert/key (e.g., from a server-generated
	// agent.conf with tls_ca_data only), that's valid — the agent will get
	// its own cert during registration via mTLS cert issuance. Don't
	// auto-generate a new CA that would override the server's CA.
	if cfg.CAFile != "" && role == "agent" {
		log.Info("TLS CA configured but no client cert — agent will obtain a cert during registration",
			"ca", cfg.CAFile)
		return cfg, nil
	}

	// No user certs — auto-generate self-signed.
	log.Warn("No TLS certs configured — auto-generating self-signed certificates. " +
		"Set DIRQ_TLS_CERT and DIRQ_TLS_KEY for production use.")
	log.Warn("SECURITY: auto-generated certs protect against passive sniffing but are " +
		"vulnerable to MITM if an attacker is on-path during agent registration. " +
		"For production, use dirq cert generate and distribute the CA cert to all agents.")

	result, err := GenerateSelfSigned(autoGenDir())
	if err != nil {
		return cfg, fmt.Errorf("auto-generate TLS certs: %w", err)
	}

	// Use the auto-generated CA for verification instead of skipping
	// verification entirely. Server and agent share the same auto-gen
	// directory, so they use the same CA.
	cfg.CAFile = result.CAFile
	cfg.CAKeyFile = result.CAKeyFile

	switch role {
	case "server":
		cfg.CertFile = result.ServerCertFile
		cfg.KeyFile = result.ServerKeyFile
	case "agent":
		cfg.CertFile = result.AgentCertFile
		cfg.KeyFile = result.AgentKeyFile
	default:
		cfg.CertFile = result.ServerCertFile
		cfg.KeyFile = result.ServerKeyFile
	}

	log.Info("auto-generated self-signed TLS certs",
		"dir", autoGenDir(),
		"cert", filepath.Base(cfg.CertFile),
		"ca", filepath.Base(cfg.CAFile),
	)

	return cfg, nil
}

// ServerCredentials returns gRPC transport credentials for a server.
// If mTLS is configured (CA provided), clients must present valid certificates.
func ServerCredentials(cfg Config) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load server cert: %w", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	// If CA is provided and not in insecure mode, require mTLS.
	if cfg.CAFile != "" && !cfg.Insecure {
		caPool, err := loadCAPool(cfg)
		if err != nil {
			return nil, err
		}
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		tlsCfg.ClientCAs = caPool
	}

	return credentials.NewTLS(tlsCfg), nil
}

// ServerCredentialsMixedAuthWithReloader returns gRPC transport credentials
// that verify client certs if presented but don't require them at the TLS
// layer, dynamically reloading the server certificate from disk via a
// CertReloader. This allows Register (no client cert) and AgentStream (client
// cert required) to share the same listener. A gRPC interceptor enforces
// client certs per-method.
func ServerCredentialsMixedAuthWithReloader(cfg Config, reloader *CertReloader) (credentials.TransportCredentials, error) {
	tlsCfg := &tls.Config{
		GetCertificate: reloader.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}

	if cfg.CAFile != "" && !cfg.Insecure {
		caPool, err := loadCAPool(cfg)
		if err != nil {
			return nil, err
		}
		tlsCfg.ClientAuth = tls.VerifyClientCertIfGiven
		tlsCfg.ClientCAs = caPool
	}

	return credentials.NewTLS(tlsCfg), nil
}

// ClientCredentials returns gRPC transport credentials for a client (agent).
// If Insecure is set, server cert verification is skipped (for self-signed).
func ClientCredentials(cfg Config) (credentials.TransportCredentials, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	// Load client certificate if provided (for mTLS).
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	// Load CA for server verification.
	if cfg.CAFile != "" && !cfg.Insecure {
		caPool, err := loadCAPool(cfg)
		if err != nil {
			return nil, err
		}
		tlsCfg.RootCAs = caPool
	}

	if cfg.Insecure {
		tlsCfg.InsecureSkipVerify = true
	}

	if cfg.ServerName != "" {
		tlsCfg.ServerName = cfg.ServerName
	}

	// Pin the peer's identity when requested. This runs in addition to the
	// standard CA-chain + hostname verification above (crypto/tls invokes
	// VerifyConnection only after that succeeds), so it tightens — never
	// loosens — verification: the parent must both hold a CA-issued cert and
	// carry the exact CN the server told us to expect.
	if cfg.ExpectedPeerCN != "" {
		want := cfg.ExpectedPeerCN
		tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
			var leaf *x509.Certificate
			if len(cs.VerifiedChains) > 0 && len(cs.VerifiedChains[0]) > 0 {
				leaf = cs.VerifiedChains[0][0]
			} else if len(cs.PeerCertificates) > 0 {
				// Only reachable under Insecure (chain verification off); still
				// enforce the CN so the pin isn't silently dropped.
				leaf = cs.PeerCertificates[0]
			}
			if leaf == nil {
				return fmt.Errorf("peer presented no certificate to verify parent identity %q", want)
			}
			if leaf.Subject.CommonName != want {
				return fmt.Errorf("parent identity mismatch: peer cert CN %q, expected %q", leaf.Subject.CommonName, want)
			}
			return nil
		}
	}

	return credentials.NewTLS(tlsCfg), nil
}

func loadCAPool(cfg Config) (*x509.CertPool, error) {
	files := []string{cfg.CAFile}
	if cfg.CAFileOld != "" {
		files = append(files, cfg.CAFileOld)
	}
	return loadCAPoolFromFiles(files...)
}

// loadCAPoolFromFiles loads CA certificates from multiple files into a single pool.
func loadCAPoolFromFiles(caFiles ...string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	for _, f := range caFiles {
		caPEM, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read CA cert %s: %w", f, err)
		}
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("failed to parse CA cert from %s", f)
		}
	}
	return pool, nil
}

// CertReloader holds a TLS certificate that can be reloaded from disk
// without restarting. Use GetCertificate for server-side TLS and
// GetClientCertificate for client-side mTLS.
type CertReloader struct {
	mu       sync.RWMutex
	certFile string
	keyFile  string
	cert     *tls.Certificate
}

// NewCertReloader creates a CertReloader and loads the initial certificate.
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload re-reads the certificate and key from disk.
func (r *CertReloader) Reload() error {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("reload cert: %w", err)
	}
	r.mu.Lock()
	r.cert = &cert
	r.mu.Unlock()
	return nil
}

// GetCertificate implements the tls.Config.GetCertificate callback.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert, nil
}

// GetClientCertificate implements the tls.Config.GetClientCertificate callback.
func (r *CertReloader) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert, nil
}
