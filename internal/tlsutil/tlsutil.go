// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

// Package tlsutil provides TLS configuration helpers for DirQ server and agents.
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

// Config holds TLS file paths loaded from environment variables.
type Config struct {
	CAFile   string // path to CA certificate
	CertFile string // path to this process's certificate
	KeyFile  string // path to this process's private key
	Insecure bool   // skip cert verification (for self-signed)
}

// Enabled returns true if TLS is configured (cert and key are set).
func (c Config) Enabled() bool {
	return c.CertFile != "" && c.KeyFile != ""
}

// ConfigFromEnv reads TLS configuration from environment variables.
func ConfigFromEnv() Config {
	return Config{
		CAFile:   os.Getenv("DIRQ_TLS_CA"),
		CertFile: os.Getenv("DIRQ_TLS_CERT"),
		KeyFile:  os.Getenv("DIRQ_TLS_KEY"),
		Insecure: os.Getenv("DIRQ_TLS_INSECURE") == "true",
	}
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

	// If CA is provided, require and verify client certificates (mTLS).
	if cfg.CAFile != "" {
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
	if cfg.CAFile != "" {
		caPool, err := loadCAPool(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		tlsCfg.RootCAs = caPool
	}

	if cfg.Insecure {
		tlsCfg.InsecureSkipVerify = true
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
