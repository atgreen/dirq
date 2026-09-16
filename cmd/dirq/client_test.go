// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// reset clears the package-level client state these tests mutate, so each
// runs against a clean CLI rather than whatever the previous one left.
func reset(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_httpClient, tlsRoots, tlsCA, tlsInsecure = nil, nil, "", false
	})
	_httpClient, tlsRoots, tlsCA, tlsInsecure = nil, nil, "", false
}

// writeCA generates a real self-signed CA and writes it as PEM. Generated
// rather than pasted: a hand-written certificate is not a certificate, and
// AppendCertsFromPEM will rightly refuse it.
func writeCA(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "dirq test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "ca.crt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadTLSRoots_NoCAIsANoOp(t *testing.T) {
	reset(t)

	if err := loadTLSRoots(false); err != nil {
		t.Fatalf("loadTLSRoots with no CA: %v", err)
	}
	if tlsRoots != nil {
		t.Error("a CA pool was built when no --tls-ca was given")
	}
}

// A bad CA path must fail loudly. Falling back to the system trust store
// would do something other than what was asked and look like it worked.
func TestLoadTLSRoots_RejectsAnUnreadableCA(t *testing.T) {
	reset(t)
	tlsCA = filepath.Join(t.TempDir(), "absent.crt")

	err := loadTLSRoots(false)
	if err == nil {
		t.Fatal("a missing --tls-ca file was accepted")
	}
	if tlsRoots != nil {
		t.Error("a CA pool was built from a file that does not exist")
	}
}

func TestLoadTLSRoots_RejectsAFileWithNoCertificates(t *testing.T) {
	reset(t)
	path := filepath.Join(t.TempDir(), "notacert")
	if err := os.WriteFile(path, []byte("this is not a certificate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tlsCA = path

	if err := loadTLSRoots(false); err == nil {
		t.Fatal("a file containing no PEM certificates was accepted")
	}
}

// TestLoadTLSRoots_CAOverridesInsecure pins the precedence: naming a CA is
// the more specific instruction, and it must win over a blanket "skip
// verification" inherited from a config file.
func TestLoadTLSRoots_CAOverridesInsecure(t *testing.T) {
	reset(t)
	tlsCA = writeCA(t)
	tlsInsecure = true

	if err := loadTLSRoots(false); err != nil {
		t.Fatalf("loadTLSRoots: %v", err)
	}
	if tlsInsecure {
		t.Error("--tls-insecure survived an explicit --tls-ca")
	}
	if tlsRoots == nil {
		t.Fatal("no CA pool was built")
	}
}

// TestHTTPClientVerifiesWhenGivenACA is the security property the whole
// change exists for: with a CA configured, the client must actually check
// the server's certificate.
func TestHTTPClientVerifiesWhenGivenACA(t *testing.T) {
	reset(t)
	tlsCA = writeCA(t)

	if err := loadTLSRoots(false); err != nil {
		t.Fatalf("loadTLSRoots: %v", err)
	}
	cfg := clientTLSConfig(t, httpClient())

	if cfg == nil {
		t.Fatal("client has no TLS config")
	}
	if cfg.InsecureSkipVerify {
		t.Error("client skips verification despite a CA being configured")
	}
	if cfg.RootCAs == nil {
		t.Error("client was not given the CA pool")
	}
}

func TestHTTPClientSkipsVerificationOnlyWhenAsked(t *testing.T) {
	reset(t)
	tlsInsecure = true

	if err := loadTLSRoots(false); err != nil {
		t.Fatal(err)
	}
	cfg := clientTLSConfig(t, httpClient())
	if cfg == nil || !cfg.InsecureSkipVerify {
		t.Error("--tls-insecure did not produce an insecure client")
	}
}

// clientTLSConfig digs the TLS config out of a client's transport.
func clientTLSConfig(t *testing.T, c *http.Client) *tls.Config {
	t.Helper()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		return nil // http.DefaultClient — system trust store
	}
	return tr.TLSClientConfig
}
