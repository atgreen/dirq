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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/atgreen/dirq/internal/config"
	"google.golang.org/grpc/credentials"
)

// Config holds TLS file paths loaded from environment variables.
type Config struct {
	CAFile     string // path to CA certificate
	CertFile   string // path to this process's certificate
	KeyFile    string // path to this process's private key
	Insecure   bool   // skip cert verification (for self-signed)
	Disabled   bool   // explicitly disable TLS (DIRQ_TLS_DISABLED=true)
	ServerName string // override TLS ServerName for verification (peer connections)
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
func ConfigFromEnv(fileCfg ...*config.File) Config {
	var fc *config.File
	if len(fileCfg) > 0 {
		fc = fileCfg[0]
	}
	return Config{
		CAFile:   config.EnvOr("DIRQ_TLS_CA", fc, "tls_ca", ""),
		CertFile: config.EnvOr("DIRQ_TLS_CERT", fc, "tls_cert", ""),
		KeyFile:  config.EnvOr("DIRQ_TLS_KEY", fc, "tls_key", ""),
		Insecure: config.EnvOr("DIRQ_TLS_INSECURE", fc, "tls_insecure", "false") == "true",
		Disabled: config.EnvOr("DIRQ_TLS_DISABLED", fc, "tls_disabled", "false") == "true",
	}
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

	// No user certs — auto-generate self-signed.
	log.Warn("No TLS certs configured — auto-generating self-signed certificates. " +
		"Set DIRQ_TLS_CERT and DIRQ_TLS_KEY for production use.")
	log.Warn("SECURITY: auto-generated certs protect against passive sniffing but are " +
		"vulnerable to MITM if an attacker is on-path during agent registration. " +
		"For production, use dirq tls generate and distribute the CA cert to all agents.")

	result, err := GenerateSelfSigned(autoGenDir())
	if err != nil {
		return cfg, fmt.Errorf("auto-generate TLS certs: %w", err)
	}

	// Use the auto-generated CA for verification instead of skipping
	// verification entirely. Server and agent share the same auto-gen
	// directory, so they use the same CA.
	cfg.CAFile = result.CAFile

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
		caPool, err := loadCAPool(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
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
		caPool, err := loadCAPool(cfg.CAFile)
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

	return credentials.NewTLS(tlsCfg), nil
}

func loadCAPool(caFile string) (*x509.CertPool, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("failed to parse CA cert from %s", caFile)
	}
	return pool, nil
}
