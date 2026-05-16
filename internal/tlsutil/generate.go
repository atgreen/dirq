// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// GenerateResult holds the paths to generated certificate files.
type GenerateResult struct {
	CAFile         string
	CAKeyFile      string
	ServerCertFile string
	ServerKeyFile  string
	AgentCertFile  string
	AgentKeyFile   string
}

// GenerateSelfSigned creates a self-signed CA and uses it to sign server and
// agent certificates. All files are written to dir.
//
// The CA is valid for 10 years. Server and agent certs are valid for 1 year.
// Server cert includes localhost and 127.0.0.1 as SANs.
// Agent cert includes a wildcard DNS SAN for peer discovery.
func GenerateSelfSigned(dir string) (*GenerateResult, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create dir: %w", err)
	}

	// If all cert files already exist, reuse them. This avoids regenerating
	// a new CA when server and agent share the same auto-gen directory —
	// both must use the same CA for mutual verification.
	result := &GenerateResult{
		CAFile:         filepath.Join(dir, "ca.crt"),
		CAKeyFile:      filepath.Join(dir, "ca.key"),
		ServerCertFile: filepath.Join(dir, "server.crt"),
		ServerKeyFile:  filepath.Join(dir, "server.key"),
		AgentCertFile:  filepath.Join(dir, "agent.crt"),
		AgentKeyFile:   filepath.Join(dir, "agent.key"),
	}
	if filesExist(result.CAFile, result.CAKeyFile,
		result.ServerCertFile, result.ServerKeyFile,
		result.AgentCertFile, result.AgentKeyFile) {
		return result, nil
	}

	// Generate CA.
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"DirQ"},
			CommonName:   "DirQ CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %w", err)
	}

	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}

	// Write CA files.
	if err := writePEM(result.CAFile, "CERTIFICATE", caCertDER); err != nil {
		return nil, err
	}
	if err := writeKeyPEM(result.CAKeyFile, caKey); err != nil {
		return nil, err
	}

	// Generate server cert.
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate server key: %w", err)
	}

	// Include all host IPs and hostname in server cert SANs so agents
	// can connect by any address without TLS verification failures.
	serverIPs := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	serverDNS := []string{"localhost", "dirq-server", "*.dirq-server"}
	if hostname, err := os.Hostname(); err == nil {
		serverDNS = append(serverDNS, hostname)
	}
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, _ := iface.Addrs()
			for _, addr := range addrs {
				if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil {
					serverIPs = append(serverIPs, ipNet.IP)
				}
			}
		}
	}

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Organization: []string{"DirQ"},
			CommonName:   "DirQ Server",
		},
		DNSNames:    serverDNS,
		IPAddresses: serverIPs,
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}

	serverCertDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create server cert: %w", err)
	}

	if err := writePEM(result.ServerCertFile, "CERTIFICATE", serverCertDER); err != nil {
		return nil, err
	}
	if err := writeKeyPEM(result.ServerKeyFile, serverKey); err != nil {
		return nil, err
	}

	// Generate agent cert.
	agentKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate agent key: %w", err)
	}

	agentTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject: pkix.Name{
			Organization: []string{"DirQ"},
			CommonName:   "DirQ Agent",
		},
		DNSNames:    []string{"localhost", "*"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}

	agentCertDER, err := x509.CreateCertificate(rand.Reader, agentTemplate, caCert, &agentKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create agent cert: %w", err)
	}

	if err := writePEM(result.AgentCertFile, "CERTIFICATE", agentCertDER); err != nil {
		return nil, err
	}
	if err := writeKeyPEM(result.AgentKeyFile, agentKey); err != nil {
		return nil, err
	}

	return result, nil
}

func filesExist(paths ...string) bool {
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}

func writePEM(path, blockType string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: data})
}

func writeKeyPEM(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}
