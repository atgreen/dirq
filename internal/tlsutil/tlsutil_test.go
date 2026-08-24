// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package tlsutil

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// helperGenerateCA creates a CA cert and key in dir, returning the file paths.
func helperGenerateCA(t *testing.T, dir string) (caFile, caKeyFile string) {
	t.Helper()
	result, err := GenerateSelfSigned(dir)
	if err != nil {
		t.Fatalf("GenerateSelfSigned: %v", err)
	}
	return result.CAFile, result.CAKeyFile
}

// helperMakeExpiringSoonCert creates a PEM cert that expires in the given duration.
func helperMakeExpiringSoonCert(t *testing.T, ttl time.Duration) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(ttl),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// --- IssueCert ---

func TestIssueCert(t *testing.T) {
	dir := t.TempDir()
	caFile, caKeyFile := helperGenerateCA(t, dir)

	caCert, caKey, err := LoadCA(caFile, caKeyFile)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}

	agentID := "agent-42"
	certPEM, keyPEM, caCertPEM, err := IssueCert(caCert, caKey, agentID)
	if err != nil {
		t.Fatalf("IssueCert: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 || len(caCertPEM) == 0 {
		t.Fatal("IssueCert returned empty PEM data")
	}

	// Parse the issued cert.
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("no PEM block in issued cert")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse issued cert: %v", err)
	}

	// Check CN matches agentID.
	if cert.Subject.CommonName != agentID {
		t.Errorf("CN = %q, want %q", cert.Subject.CommonName, agentID)
	}

	// Verify signed by the CA.
	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)
	opts := x509.VerifyOptions{
		Roots:     caPool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if _, err := cert.Verify(opts); err != nil {
		t.Errorf("cert not signed by CA: %v", err)
	}

	// Check ExtKeyUsage includes both ClientAuth and ServerAuth.
	hasClient, hasServer := false, false
	for _, u := range cert.ExtKeyUsage {
		if u == x509.ExtKeyUsageClientAuth {
			hasClient = true
		}
		if u == x509.ExtKeyUsageServerAuth {
			hasServer = true
		}
	}
	if !hasClient {
		t.Error("issued cert missing ExtKeyUsageClientAuth")
	}
	if !hasServer {
		t.Error("issued cert missing ExtKeyUsageServerAuth")
	}
}

// --- LoadCA ---

func TestLoadCA_Success(t *testing.T) {
	dir := t.TempDir()
	caFile, caKeyFile := helperGenerateCA(t, dir)

	caCert, caKey, err := LoadCA(caFile, caKeyFile)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if caCert == nil {
		t.Error("caCert is nil")
	}
	if caKey == nil {
		t.Error("caKey is nil")
	}
	if caCert.Subject.CommonName != "DirQ CA" {
		t.Errorf("CA CN = %q, want %q", caCert.Subject.CommonName, "DirQ CA")
	}
	if !caCert.IsCA {
		t.Error("CA cert does not have IsCA set")
	}
}

func TestLoadCA_MissingFiles(t *testing.T) {
	dir := t.TempDir()
	_, _, err := LoadCA(filepath.Join(dir, "nonexistent.crt"), filepath.Join(dir, "nonexistent.key"))
	if err == nil {
		t.Error("expected error for missing CA cert file")
	}

	// Create a valid CA cert but point to a missing key file.
	caFile, _ := helperGenerateCA(t, dir)
	_, _, err = LoadCA(caFile, filepath.Join(dir, "nonexistent.key"))
	if err == nil {
		t.Error("expected error for missing CA key file")
	}
}

func TestLoadCA_InvalidPEM(t *testing.T) {
	dir := t.TempDir()
	badFile := filepath.Join(dir, "bad.pem")
	os.WriteFile(badFile, []byte("not a pem"), 0644)

	_, _, err := LoadCA(badFile, badFile)
	if err == nil {
		t.Error("expected error for invalid PEM")
	}
}

// --- CertExpiresWithin ---

func TestCertExpiresWithin_NearExpiry(t *testing.T) {
	// Cert expires in 1 hour — should be within 24h window.
	certPEM := helperMakeExpiringSoonCert(t, 1*time.Hour)
	if !CertExpiresWithin(certPEM, 24*time.Hour) {
		t.Error("expected CertExpiresWithin to return true for cert expiring in 1h with 24h window")
	}
}

func TestCertExpiresWithin_FarFromExpiry(t *testing.T) {
	// Cert expires in 365 days — should not be within 24h window.
	certPEM := helperMakeExpiringSoonCert(t, 365*24*time.Hour)
	if CertExpiresWithin(certPEM, 24*time.Hour) {
		t.Error("expected CertExpiresWithin to return false for cert expiring in 365d with 24h window")
	}
}

func TestCertExpiresWithin_InvalidPEM(t *testing.T) {
	if !CertExpiresWithin([]byte("garbage"), 24*time.Hour) {
		t.Error("expected CertExpiresWithin to return true for invalid PEM")
	}
}

// --- CertCN ---

func TestCertCN(t *testing.T) {
	dir := t.TempDir()
	caFile, caKeyFile := helperGenerateCA(t, dir)

	caCert, caKey, err := LoadCA(caFile, caKeyFile)
	if err != nil {
		t.Fatal(err)
	}

	certPEM, _, _, err := IssueCert(caCert, caKey, "my-agent")
	if err != nil {
		t.Fatal(err)
	}

	cn := CertCN(certPEM)
	if cn != "my-agent" {
		t.Errorf("CertCN = %q, want %q", cn, "my-agent")
	}
}

func TestCertCN_InvalidPEM(t *testing.T) {
	cn := CertCN([]byte("not valid pem"))
	if cn != "" {
		t.Errorf("CertCN on invalid PEM = %q, want empty", cn)
	}
}

func TestCertCN_EmptyInput(t *testing.T) {
	cn := CertCN(nil)
	if cn != "" {
		t.Errorf("CertCN on nil = %q, want empty", cn)
	}
}

// --- GenerateSelfSigned ---

func TestGenerateSelfSigned_CreatesAllFiles(t *testing.T) {
	dir := t.TempDir()
	result, err := GenerateSelfSigned(dir)
	if err != nil {
		t.Fatalf("GenerateSelfSigned: %v", err)
	}

	files := []string{
		result.CAFile, result.CAKeyFile,
		result.ServerCertFile, result.ServerKeyFile,
		result.AgentCertFile, result.AgentKeyFile,
	}
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			t.Errorf("expected file %s to exist: %v", f, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("file %s is empty", f)
		}
	}
}

func TestGenerateSelfSigned_ReusesExisting(t *testing.T) {
	dir := t.TempDir()

	result1, err := GenerateSelfSigned(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Record modification times.
	modTimes := make(map[string]time.Time)
	for _, f := range []string{
		result1.CAFile, result1.CAKeyFile,
		result1.ServerCertFile, result1.ServerKeyFile,
		result1.AgentCertFile, result1.AgentKeyFile,
	} {
		info, _ := os.Stat(f)
		modTimes[f] = info.ModTime()
	}

	// Call again — should reuse.
	result2, err := GenerateSelfSigned(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Paths should match.
	if result1.CAFile != result2.CAFile {
		t.Error("CAFile path changed on second call")
	}

	// Modification times should be unchanged.
	for _, f := range []string{
		result2.CAFile, result2.CAKeyFile,
		result2.ServerCertFile, result2.ServerKeyFile,
		result2.AgentCertFile, result2.AgentKeyFile,
	} {
		info, _ := os.Stat(f)
		if !info.ModTime().Equal(modTimes[f]) {
			t.Errorf("file %s was regenerated (mod time changed)", filepath.Base(f))
		}
	}
}

func TestGenerateSelfSigned_SubDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "deep")
	_, err := GenerateSelfSigned(dir)
	if err != nil {
		t.Fatalf("GenerateSelfSigned with nested dir: %v", err)
	}
}

// --- ServerCredentialsMixedAuth vs ServerCredentials ---

func TestServerCredentialsMixedAuth_NoClientCertRequired(t *testing.T) {
	dir := t.TempDir()
	result, err := GenerateSelfSigned(dir)
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		CAFile:   result.CAFile,
		CertFile: result.ServerCertFile,
		KeyFile:  result.ServerKeyFile,
	}

	// ServerCredentials should require client certs (mTLS).
	creds, err := ServerCredentials(cfg)
	if err != nil {
		t.Fatalf("ServerCredentials: %v", err)
	}
	// Extract the tls.Config to verify ClientAuth.
	tlsInfo := creds.Info()
	if tlsInfo.SecurityProtocol != "tls" {
		t.Errorf("ServerCredentials protocol = %q, want tls", tlsInfo.SecurityProtocol)
	}

	// ServerCredentialsMixedAuthWithReloader should not require client certs
	// at the TLS layer.
	reloader, err := NewCertReloader(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	mixedCreds, err := ServerCredentialsMixedAuthWithReloader(cfg, reloader)
	if err != nil {
		t.Fatalf("ServerCredentialsMixedAuthWithReloader: %v", err)
	}
	mixedInfo := mixedCreds.Info()
	if mixedInfo.SecurityProtocol != "tls" {
		t.Errorf("ServerCredentialsMixedAuthWithReloader protocol = %q, want tls", mixedInfo.SecurityProtocol)
	}

	// To verify the actual ClientAuth difference, do a real TLS handshake.
	// ServerCredentials with CA should require client cert.
	// Mixed auth with CA should accept without client cert.
	testMixedAuthHandshake(t, result)
}

func testMixedAuthHandshake(t *testing.T, result *GenerateResult) {
	t.Helper()

	cfg := Config{
		CAFile:   result.CAFile,
		CertFile: result.ServerCertFile,
		KeyFile:  result.ServerKeyFile,
	}

	// Set up a listener with mixed auth.
	reloader, err := NewCertReloader(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	mixedCreds, err := ServerCredentialsMixedAuthWithReloader(cfg, reloader)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Accept one connection in background.
	serverDone := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		sConn, _, err := mixedCreds.ServerHandshake(conn)
		if err != nil {
			serverDone <- err
			return
		}
		sConn.Close()
		serverDone <- nil
	}()

	// Client connects without a client cert — should succeed with mixed auth.
	caPEM, _ := os.ReadFile(result.CAFile)
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caPEM)

	clientConn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		RootCAs:    caPool,
		MinVersion: tls.VersionTLS12,
		ServerName: "localhost",
		NextProtos: []string{"h2"}, // gRPC requires ALPN with h2
	})
	if err != nil {
		t.Fatalf("client dial without client cert should succeed for mixed auth: %v", err)
	}
	clientConn.Close()

	if err := <-serverDone; err != nil {
		t.Fatalf("server handshake failed: %v", err)
	}
}

// --- EnsureCerts with role=agent and CAFile set but no cert/key ---

func TestEnsureCerts_AgentWithCAOnly(t *testing.T) {
	dir := t.TempDir()
	result, err := GenerateSelfSigned(dir)
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		CAFile: result.CAFile,
		// No CertFile or KeyFile — agent should return early.
	}

	log := newDiscardLogger()
	out, err := EnsureCerts(cfg, "agent", log)
	if err != nil {
		t.Fatalf("EnsureCerts: %v", err)
	}

	// Should return the config unchanged — no cert/key auto-generated.
	if out.CertFile != "" {
		t.Errorf("expected empty CertFile, got %q", out.CertFile)
	}
	if out.KeyFile != "" {
		t.Errorf("expected empty KeyFile, got %q", out.KeyFile)
	}
	// CA should be preserved.
	if out.CAFile != result.CAFile {
		t.Errorf("CAFile changed: got %q, want %q", out.CAFile, result.CAFile)
	}
}

func TestEnsureCerts_Disabled(t *testing.T) {
	cfg := Config{Disabled: true}
	log := newDiscardLogger()
	out, err := EnsureCerts(cfg, "server", log)
	if err != nil {
		t.Fatalf("EnsureCerts: %v", err)
	}
	if !out.Disabled {
		t.Error("expected Disabled to remain true")
	}
}

func TestEnsureCerts_UserCerts(t *testing.T) {
	dir := t.TempDir()
	result, err := GenerateSelfSigned(dir)
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		CAFile:   result.CAFile,
		CertFile: result.ServerCertFile,
		KeyFile:  result.ServerKeyFile,
	}

	log := newDiscardLogger()
	out, err := EnsureCerts(cfg, "server", log)
	if err != nil {
		t.Fatalf("EnsureCerts: %v", err)
	}
	// Should pass through unchanged.
	if out.CertFile != cfg.CertFile {
		t.Errorf("CertFile changed")
	}
}

// --- ClientCredentials parent-identity pinning ---

// helperIssueAgentCert issues a per-agent cert (CN = agentID) signed by the
// given CA and writes it to temp files, returning their paths.
func helperIssueAgentCert(t *testing.T, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, agentID string) (certFile, keyFile string) {
	t.Helper()
	certPEM, keyPEM, _, err := IssueCert(caCert, caKey, agentID)
	if err != nil {
		t.Fatalf("IssueCert(%s): %v", agentID, err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// TestClientCredentials_ParentIDPin verifies that ExpectedPeerCN pins the
// identity of the server side (the parent relay): a child accepts only the
// specific parent the server assigned it and rejects any other CA-issued agent
// cert. This is the control that stops a rogue agent — which legitimately holds
// a CA-issued cert — from impersonating a parent relay and hijacking a child's
// uplink. Empty ExpectedPeerCN falls back to CA-only verification (used for
// fallback parents and mixed-version rollout, where the parent ID is unknown).
func TestClientCredentials_ParentIDPin(t *testing.T) {
	dir := t.TempDir()
	caFile, caKeyFile := helperGenerateCA(t, dir)
	caCert, caKey, err := LoadCA(caFile, caKeyFile)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}

	// The parent relay presents a per-agent cert with CN = "parent-A".
	parentCert, parentKey := helperIssueAgentCert(t, caCert, caKey, "parent-A")
	parentReloader, err := NewCertReloader(parentCert, parentKey)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	serverCreds, err := ServerCredentialsMixedAuthWithReloader(Config{CAFile: caFile, CertFile: parentCert, KeyFile: parentKey}, parentReloader)
	if err != nil {
		t.Fatalf("ServerCredentialsMixedAuthWithReloader: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sConn, _, err := serverCreds.ServerHandshake(conn)
				if err != nil {
					conn.Close()
					return
				}
				sConn.Close()
			}()
		}
	}()

	dialWithPin := func(expectCN string) error {
		creds, err := ClientCredentials(Config{CAFile: caFile, ServerName: "localhost", ExpectedPeerCN: expectCN})
		if err != nil {
			return err
		}
		raw, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, _, err := creds.ClientHandshake(ctx, "localhost", raw)
		if conn != nil {
			conn.Close()
		}
		return err
	}

	// Pinning to the assigned parent is accepted.
	if err := dialWithPin("parent-A"); err != nil {
		t.Fatalf("pinning to the assigned parent should succeed: %v", err)
	}

	// Pinning to a different parent is rejected — the impersonation case: the
	// relay holds a valid CA-issued cert, but not the identity the child expects.
	err = dialWithPin("parent-B")
	if err == nil {
		t.Fatal("pinning to a different parent should fail, but the handshake succeeded")
	}
	if !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("expected an identity-mismatch rejection, got: %v", err)
	}

	// No pin (empty ExpectedPeerCN) falls back to CA-only verification and succeeds.
	if err := dialWithPin(""); err != nil {
		t.Fatalf("CA-only verification (no pin) should succeed: %v", err)
	}
}
